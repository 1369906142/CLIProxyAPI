package redisstate

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	store, err := New(context.Background(), config.RedisConfig{
		Addr:      mini.Addr(),
		KeyPrefix: "test-redisstate",
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func TestRateLimitTokenBucket(t *testing.T) {
	store := newTestStore(t)

	first, err := store.RateLimit(context.Background(), "tenant:alpha", 1, 1)
	if err != nil {
		t.Fatalf("first rate limit call: %v", err)
	}
	if !first.Allowed {
		t.Fatal("expected first request to be allowed")
	}

	second, err := store.RateLimit(context.Background(), "tenant:alpha", 1, 1)
	if err != nil {
		t.Fatalf("second rate limit call: %v", err)
	}
	if second.Allowed {
		t.Fatal("expected second request to be denied")
	}
	if second.RetryAfter <= 0 {
		t.Fatalf("expected retry-after > 0, got %v", second.RetryAfter)
	}
}

func TestReserveAuthQuotaConcurrent(t *testing.T) {
	store := newTestStore(t)
	attrs := map[string]string{
		"quota_concurrent": "1",
	}

	first, err := store.ReserveAuthQuota(context.Background(), "auth-1", attrs)
	if err != nil {
		t.Fatalf("reserve first quota: %v", err)
	}
	if first == nil || !first.Allowed {
		t.Fatal("expected first reservation to succeed")
	}

	second, err := store.ReserveAuthQuota(context.Background(), "auth-1", attrs)
	if err != nil {
		t.Fatalf("reserve second quota: %v", err)
	}
	if second == nil || second.Allowed {
		t.Fatal("expected second reservation to be denied")
	}

	first.Release(context.Background())

	third, err := store.ReserveAuthQuota(context.Background(), "auth-1", attrs)
	if err != nil {
		t.Fatalf("reserve third quota: %v", err)
	}
	if third == nil || !third.Allowed {
		t.Fatal("expected third reservation to succeed after release")
	}
}

func TestSelectProxySkipsOpenCircuitCandidate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	candidates := []ProxyCandidate{
		{
			PoolID:      "pool-a",
			URL:         "socks5://proxy-a:1080",
			Weight:      1,
			MaxFailures: 1,
			Cooldown:    time.Minute,
		},
		{
			PoolID:      "pool-a",
			URL:         "socks5://proxy-b:1080",
			Weight:      1,
			MaxFailures: 1,
			Cooldown:    time.Minute,
		},
	}

	if err := store.MarkProxyResult(ctx, candidates[0], false); err != nil {
		t.Fatalf("mark proxy result: %v", err)
	}

	selected, ok, err := store.SelectProxy(ctx, "pool-a", candidates)
	if err != nil {
		t.Fatalf("select proxy: %v", err)
	}
	if !ok {
		t.Fatal("expected a proxy candidate")
	}
	if selected.URL != candidates[1].URL {
		t.Fatalf("expected healthy proxy %q, got %q", candidates[1].URL, selected.URL)
	}
}
