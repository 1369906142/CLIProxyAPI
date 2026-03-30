package redisstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

var rateLimitScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local data = redis.call("HMGET", key, "tokens", "ts")
local tokens = tonumber(data[1])
local ts = tonumber(data[2])
if tokens == nil then
  tokens = burst
end
if ts == nil then
  ts = now
end

local delta = now - ts
if delta < 0 then
  delta = 0
end

tokens = math.min(burst, tokens + (delta * rate))
local allowed = 0
local retry_after = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry_after = math.ceil((1 - tokens) / rate)
end

redis.call("HMSET", key, "tokens", tokens, "ts", now)
redis.call("EXPIRE", key, ttl)
return {allowed, retry_after, tokens}
`)

type Store struct {
	client *redis.Client
	prefix string

	mu         sync.RWMutex
	proxyCache map[string]cachedProxyState
}

type cachedProxyState struct {
	failures  int
	openUntil time.Time
	expiresAt time.Time
}

type RateLimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
	Remaining  float64
}

type ProxyCandidate struct {
	PoolID       string
	URL          string
	Weight       int
	MaxFailures  int
	Cooldown     time.Duration
	Mode         string
	ExplicitName string
}

type AuthQuotaConfig struct {
	PerMinute  int
	PerDay     int
	Concurrent int
}

type AuthQuotaReservation struct {
	Allowed    bool
	Reason     string
	RetryAfter time.Duration

	store       *Store
	authID      string
	concurrency bool
	released    bool
}

func New(ctx context.Context, cfg config.RedisConfig) (*Store, error) {
	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		return nil, fmt.Errorf("redis state: addr is required")
	}
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	if ctx == nil {
		ctx = context.Background()
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis state: ping failed: %w", err)
	}
	prefix := strings.TrimSpace(cfg.KeyPrefix)
	if prefix == "" {
		prefix = "cpa"
	}
	return &Store{
		client:     client,
		prefix:     prefix,
		proxyCache: make(map[string]cachedProxyState),
	}, nil
}

func (s *Store) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.client == nil {
		return errors.New("redis state: client is nil")
	}
	return s.client.Ping(ctx).Err()
}

func (s *Store) RateLimit(ctx context.Context, scope string, rate float64, burst int) (RateLimitDecision, error) {
	decision := RateLimitDecision{Allowed: true}
	if s == nil || s.client == nil || rate <= 0 || burst <= 0 {
		return decision, nil
	}
	now := float64(time.Now().UnixMilli()) / 1000
	key := s.key("ratelimit", scope)
	values, err := rateLimitScript.Run(ctx, s.client, []string{key}, now, rate, burst, 120).Result()
	if err != nil {
		return RateLimitDecision{}, err
	}
	items, ok := values.([]any)
	if !ok || len(items) < 3 {
		return RateLimitDecision{}, fmt.Errorf("redis state: unexpected rate limit response")
	}
	allowed := parseInt(items[0]) == 1
	retryAfterSeconds := parseInt(items[1])
	remaining := parseFloat(items[2])
	decision.Allowed = allowed
	decision.RetryAfter = time.Duration(retryAfterSeconds) * time.Second
	decision.Remaining = remaining
	return decision, nil
}

func (s *Store) ParseAuthQuota(attrs map[string]string) AuthQuotaConfig {
	if len(attrs) == 0 {
		return AuthQuotaConfig{}
	}
	return AuthQuotaConfig{
		PerMinute:  atoiPositive(attrs["quota_per_minute"]),
		PerDay:     atoiPositive(attrs["quota_per_day"]),
		Concurrent: atoiPositive(attrs["quota_concurrent"]),
	}
}

func (s *Store) ReserveAuthQuota(ctx context.Context, authID string, attrs map[string]string) (*AuthQuotaReservation, error) {
	reservation := &AuthQuotaReservation{Allowed: true}
	if s == nil || s.client == nil {
		return reservation, nil
	}
	limits := s.ParseAuthQuota(attrs)
	if limits.PerMinute <= 0 && limits.PerDay <= 0 && limits.Concurrent <= 0 {
		return reservation, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return reservation, nil
	}

	now := time.Now().UTC()
	if limits.PerMinute > 0 {
		key := s.key("authquota", authID, "minute", now.Format("200601021504"))
		count, err := s.client.Incr(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		if count == 1 {
			_ = s.client.Expire(ctx, key, 2*time.Minute).Err()
		}
		if count > int64(limits.PerMinute) {
			return &AuthQuotaReservation{
				Allowed:    false,
				Reason:     "per_minute",
				RetryAfter: time.Until(now.Truncate(time.Minute).Add(time.Minute)),
			}, nil
		}
	}

	if limits.PerDay > 0 {
		key := s.key("authquota", authID, "day", now.Format("20060102"))
		count, err := s.client.Incr(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		if count == 1 {
			nextMidnight := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
			_ = s.client.Expire(ctx, key, time.Until(nextMidnight)+time.Hour).Err()
		}
		if count > int64(limits.PerDay) {
			nextMidnight := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
			return &AuthQuotaReservation{
				Allowed:    false,
				Reason:     "per_day",
				RetryAfter: time.Until(nextMidnight),
			}, nil
		}
	}

	if limits.Concurrent > 0 {
		key := s.key("authquota", authID, "concurrent")
		count, err := s.client.Incr(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		if count == 1 {
			_ = s.client.Expire(ctx, key, time.Hour).Err()
		}
		if count > int64(limits.Concurrent) {
			_, _ = s.client.Decr(ctx, key).Result()
			return &AuthQuotaReservation{
				Allowed:    false,
				Reason:     "concurrent",
				RetryAfter: time.Second,
			}, nil
		}
		reservation.concurrency = true
	}

	reservation.store = s
	reservation.authID = authID
	return reservation, nil
}

func (r *AuthQuotaReservation) Release(ctx context.Context) {
	if r == nil || r.store == nil || !r.concurrency || r.released || r.authID == "" {
		return
	}
	r.released = true
	_, _ = r.store.client.Decr(ctx, r.store.key("authquota", r.authID, "concurrent")).Result()
}

func (s *Store) SetAuthStatus(ctx context.Context, authID, status, reason string, nextRetry time.Time) error {
	if s == nil || s.client == nil || strings.TrimSpace(authID) == "" {
		return nil
	}
	values := map[string]any{
		"status":     strings.TrimSpace(status),
		"reason":     strings.TrimSpace(reason),
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if !nextRetry.IsZero() {
		values["next_retry_at"] = nextRetry.UTC().Format(time.RFC3339Nano)
	} else {
		values["next_retry_at"] = ""
	}
	return s.client.HSet(ctx, s.key("authstatus", authID), values).Err()
}

func (s *Store) RecordUsage(ctx context.Context, record coreusage.Record) error {
	if s == nil || s.client == nil {
		return nil
	}
	now := record.RequestedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	dayKey := now.Format("20060102")
	fields := map[string]int64{
		"requests": 1,
		"success":  0,
		"failure":  0,
		"tokens":   record.Detail.TotalTokens,
		"latency":  record.Latency.Milliseconds(),
	}
	if record.Failed {
		fields["failure"] = 1
	} else {
		fields["success"] = 1
	}
	keyParts := []string{"usage", dayKey}
	if tenantID := strings.TrimSpace(record.TenantID); tenantID != "" {
		keyParts = append(keyParts, "tenant", tenantID)
	}
	if authID := strings.TrimSpace(record.AuthID); authID != "" {
		keyParts = append(keyParts, "auth", authID)
	}
	if provider := strings.TrimSpace(record.Provider); provider != "" {
		keyParts = append(keyParts, "provider", provider)
	}
	if model := strings.TrimSpace(record.Model); model != "" {
		keyParts = append(keyParts, "model", model)
	}
	pipe := s.client.Pipeline()
	for field, value := range fields {
		if value == 0 {
			continue
		}
		pipe.HIncrBy(ctx, s.key(keyParts...), field, value)
	}
	payload := fmt.Sprintf(
		`{"tenant_id":%q,"auth_id":%q,"provider":%q,"model":%q,"failed":%t,"latency_ms":%d,"tokens":%d,"requested_at":%q}`,
		record.TenantID,
		record.AuthID,
		record.Provider,
		record.Model,
		record.Failed,
		record.Latency.Milliseconds(),
		record.Detail.TotalTokens,
		now.Format(time.RFC3339Nano),
	)
	recentKey := s.key("usage", "recent")
	pipe.LPush(ctx, recentKey, payload)
	pipe.LTrim(ctx, recentKey, 0, 499)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) SelectProxy(ctx context.Context, poolID string, candidates []ProxyCandidate) (ProxyCandidate, bool, error) {
	poolID = strings.TrimSpace(poolID)
	if s == nil || s.client == nil || poolID == "" || len(candidates) == 0 {
		return ProxyCandidate{}, false, nil
	}
	now := time.Now()
	type weightedCandidate struct {
		candidate ProxyCandidate
		openUntil time.Time
	}
	healthy := make([]weightedCandidate, 0, len(candidates))
	fallback := make([]weightedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		state, err := s.proxyState(ctx, candidate)
		if err != nil {
			return ProxyCandidate{}, false, err
		}
		entry := weightedCandidate{candidate: candidate, openUntil: state.openUntil}
		if state.openUntil.After(now) {
			fallback = append(fallback, entry)
			continue
		}
		healthy = append(healthy, entry)
	}
	if len(healthy) == 0 {
		if len(fallback) == 0 {
			return ProxyCandidate{}, false, nil
		}
		selected := fallback[0]
		for _, candidate := range fallback[1:] {
			if candidate.openUntil.Before(selected.openUntil) {
				selected = candidate
			}
		}
		return selected.candidate, true, nil
	}
	totalWeight := 0
	for _, candidate := range healthy {
		weight := candidate.candidate.Weight
		if weight <= 0 {
			weight = 1
		}
		totalWeight += weight
	}
	if totalWeight <= 0 {
		return healthy[0].candidate, true, nil
	}
	pick := rand.IntN(totalWeight)
	for _, candidate := range healthy {
		weight := candidate.candidate.Weight
		if weight <= 0 {
			weight = 1
		}
		if pick < weight {
			return candidate.candidate, true, nil
		}
		pick -= weight
	}
	return healthy[0].candidate, true, nil
}

func (s *Store) MarkProxyResult(ctx context.Context, candidate ProxyCandidate, success bool) error {
	if s == nil || s.client == nil || strings.TrimSpace(candidate.PoolID) == "" || strings.TrimSpace(candidate.URL) == "" {
		return nil
	}
	key := s.proxyKey(candidate)
	now := time.Now().UTC()
	if success {
		s.setProxyCache(key, cachedProxyState{
			failures:  0,
			openUntil: time.Time{},
			expiresAt: now.Add(5 * time.Second),
		})
		return s.client.HSet(ctx, key, map[string]any{
			"failures":   0,
			"open_until": "",
			"updated_at": now.Format(time.RFC3339Nano),
		}).Err()
	}
	state, err := s.proxyState(ctx, candidate)
	if err != nil {
		return err
	}
	failures := state.failures + 1
	openUntil := time.Time{}
	maxFailures := candidate.MaxFailures
	if maxFailures <= 0 {
		maxFailures = 3
	}
	cooldown := candidate.Cooldown
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if failures >= maxFailures {
		openUntil = now.Add(cooldown)
	}
	s.setProxyCache(key, cachedProxyState{
		failures:  failures,
		openUntil: openUntil,
		expiresAt: now.Add(5 * time.Second),
	})
	values := map[string]any{
		"failures":   failures,
		"updated_at": now.Format(time.RFC3339Nano),
	}
	if !openUntil.IsZero() {
		values["open_until"] = openUntil.Format(time.RFC3339Nano)
	} else {
		values["open_until"] = ""
	}
	return s.client.HSet(ctx, key, values).Err()
}

func (s *Store) proxyState(ctx context.Context, candidate ProxyCandidate) (cachedProxyState, error) {
	key := s.proxyKey(candidate)
	if state, ok := s.getProxyCache(key); ok {
		return state, nil
	}
	values, err := s.client.HGetAll(ctx, key).Result()
	if err != nil {
		return cachedProxyState{}, err
	}
	state := cachedProxyState{expiresAt: time.Now().Add(5 * time.Second)}
	state.failures = atoiPositive(values["failures"])
	if raw := strings.TrimSpace(values["open_until"]); raw != "" {
		if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
			state.openUntil = parsed
		}
	}
	s.setProxyCache(key, state)
	return state, nil
}

func (s *Store) getProxyCache(key string) (cachedProxyState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.proxyCache[key]
	if !ok || time.Now().After(state.expiresAt) {
		return cachedProxyState{}, false
	}
	return state, true
}

func (s *Store) setProxyCache(key string, state cachedProxyState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxyCache[key] = state
}

func (s *Store) proxyKey(candidate ProxyCandidate) string {
	return s.key("proxyhealth", candidate.PoolID, hashString(candidate.URL))
}

func (s *Store) key(parts ...string) string {
	all := make([]string, 0, len(parts)+1)
	all = append(all, s.prefix)
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			all = append(all, trimmed)
		}
	}
	return strings.Join(all, ":")
}

func parseInt(raw any) int {
	switch value := raw.(type) {
	case int64:
		return int(value)
	case float64:
		return int(value)
	case string:
		return atoiPositive(value)
	default:
		return 0
	}
}

func parseFloat(raw any) float64 {
	switch value := raw.(type) {
	case int64:
		return float64(value)
	case float64:
		return value
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed
	default:
		return 0
	}
}

func atoiPositive(raw string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func hashString(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:8])
}

func HashKey(raw string) string {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(strings.TrimSpace(raw)))
	return fmt.Sprintf("%x", hasher.Sum64())
}
