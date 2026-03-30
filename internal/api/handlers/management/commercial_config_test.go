package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	gatewaycfg "github.com/router-for-me/CLIProxyAPI/v6/internal/gateway"
)

func TestPutCommercialConfigPersistsRedisAndProxyPools(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("proxy-url: \"\"\nrequest-retry: 0\nmax-retry-interval: 0\ndisable-cooling: false\nrouting:\n  strategy: round-robin\nredis: {}\ntransport: {}\n"), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.configFilePath = configPath

	body := map[string]any{
		"proxy-url":          "http://127.0.0.1:7890",
		"request-retry":      3,
		"max-retry-interval": 120,
		"disable-cooling":    true,
		"routing": map[string]any{
			"strategy": "fill-first",
		},
		"redis": map[string]any{
			"addr":       "127.0.0.1:6379",
			"db":         2,
			"key-prefix": "cpa:test",
		},
		"transport": map[string]any{
			"proxy-pools": []map[string]any{
				{
					"id":               "pool-a",
					"url":              "socks5://127.0.0.1:1080",
					"weight":           2,
					"max-failures":     5,
					"cooldown-seconds": 90,
					"mode":             "weighted",
				},
			},
		},
	}
	raw, _ := json.Marshal(body)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/commercial/config", bytes.NewReader(raw))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutCommercialConfig(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if h.cfg.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("proxy url = %q", h.cfg.ProxyURL)
	}
	if h.cfg.Routing.Strategy != "fill-first" {
		t.Fatalf("routing strategy = %q", h.cfg.Routing.Strategy)
	}
	if got := len(h.cfg.Transport.ProxyPools); got != 1 {
		t.Fatalf("proxy pool count = %d", got)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	if !bytes.Contains(data, []byte("proxy-pools")) {
		t.Fatalf("expected proxy-pools in persisted file, got %s", string(data))
	}
}

func TestPutGatewayConfigWritesValidatedConfig(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: false\n"), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	gatewayPath := filepath.Join(dir, "gateway.yaml")

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.configFilePath = configPath
	t.Setenv("CPA_GATEWAY_CONFIG_PATH", gatewayPath)

	payload := gatewaycfg.Config{
		Listen: ":8320",
		Backend: gatewaycfg.BackendConfig{
			URL:            "http://127.0.0.1:8315",
			InternalAPIKey: "internal-key",
		},
		Redis: config.RedisConfig{
			Addr: "127.0.0.1:6379",
		},
		Tenants: []gatewaycfg.TenantConfig{
			{
				ID:        "tenant-a",
				APIKey:    "tenant-key",
				QPS:       2,
				Burst:     4,
				TimeoutMS: 60000,
				ProxyPool: "pool-a",
				Retry: gatewaycfg.RetryConfig{
					MaxAttempts: 3,
				},
			},
		},
	}
	raw, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/gateway/config", bytes.NewReader(raw))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutGatewayConfig(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	loaded, err := gatewaycfg.LoadConfig(gatewayPath)
	if err != nil {
		t.Fatalf("load gateway config: %v", err)
	}
	if loaded.Backend.InternalAPIKey != "internal-key" {
		t.Fatalf("internal api key = %q", loaded.Backend.InternalAPIKey)
	}
	if len(loaded.Tenants) != 1 || loaded.Tenants[0].ID != "tenant-a" {
		t.Fatalf("tenants = %#v", loaded.Tenants)
	}
}
