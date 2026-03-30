package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/redisstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/requestctx"
)

func newTestGatewayServer(t *testing.T, backend http.HandlerFunc, mutate func(*Config)) (*httptest.Server, *redisstate.Store) {
	t.Helper()

	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	store, err := redisstate.New(context.Background(), internalconfig.RedisConfig{
		Addr:      mini.Addr(),
		KeyPrefix: "test-gateway",
	})
	if err != nil {
		t.Fatalf("new redis store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	backendServer := httptest.NewServer(backend)
	t.Cleanup(backendServer.Close)

	cfg := &Config{
		Listen: ":0",
		Backend: BackendConfig{
			URL:            backendServer.URL,
			InternalAPIKey: "internal-secret",
		},
		Redis: internalconfig.RedisConfig{
			Addr:      mini.Addr(),
			KeyPrefix: "test-gateway",
		},
		Tenants: []TenantConfig{
			{
				ID:        "tenant-a",
				APIKey:    "tenant-secret",
				QPS:       10,
				Burst:     10,
				TimeoutMS: 5000,
				ProxyPool: "pool-a",
				Retry: RetryConfig{
					MaxAttempts: 3,
				},
			},
		},
		Timeouts: TimeoutConfig{
			ConnectMS:        1000,
			ResponseHeaderMS: 1000,
			TotalMS:          5000,
			StreamIdleMS:     1000,
		},
	}
	cfg.ApplyDefaults()
	if mutate != nil {
		mutate(cfg)
	}

	srv, err := NewServer(cfg, store)
	if err != nil {
		t.Fatalf("new gateway server: %v", err)
	}
	return httptest.NewServer(srv.Handler()), store
}

func TestServerRejectsUnauthorizedRequests(t *testing.T) {
	server, _ := newTestGatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, nil)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestServerRetriesBeforeSuccess(t *testing.T) {
	var attempts atomic.Int32
	server, _ := newTestGatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
		current := attempts.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer internal-secret" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		if got := r.Header.Get(requestctx.HeaderTenantID); got != "tenant-a" {
			t.Fatalf("unexpected tenant header: %q", got)
		}
		if got := r.Header.Get(requestctx.HeaderProxyPool); got != "pool-a" {
			t.Fatalf("unexpected proxy pool header: %q", got)
		}
		if got := r.Header.Get(requestctx.HeaderRetryAttempt); got == "" {
			t.Fatal("expected retry attempt header")
		}
		if current == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"retry"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}, nil)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tenant-secret")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestServerRateLimitsPerTenant(t *testing.T) {
	server, _ := newTestGatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}, func(cfg *Config) {
		cfg.Tenants[0].QPS = 1
		cfg.Tenants[0].Burst = 1
	})
	defer server.Close()

	doReq := func() *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1"}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer tenant-secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		return resp
	}

	first := doReq()
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("expected first request 200, got %d", first.StatusCode)
	}

	second := doReq()
	defer second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected second request 429, got %d", second.StatusCode)
	}
}

func TestServerRetriesStreamingBeforeFirstByte(t *testing.T) {
	var attempts atomic.Int32
	server, _ := newTestGatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
		current := attempts.Add(1)
		if current == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: hello\n\n"))
	}, nil)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tenant-secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "data: hello") {
		t.Fatalf("unexpected streaming body: %s", string(body))
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestServerReadyzDependsOnRedis(t *testing.T) {
	server, store := newTestGatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, nil)
	defer server.Close()

	resp, err := http.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected readyz 200, got %d", resp.StatusCode)
	}

	_ = store.Close()
	time.Sleep(20 * time.Millisecond)

	resp, err = http.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz request after close: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz 503 after redis close, got %d", resp.StatusCode)
	}
}
