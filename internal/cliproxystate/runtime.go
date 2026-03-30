package cliproxystate

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/redisstate"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type quotaLimiter struct {
	store *redisstate.Store
}

type quotaReservation struct {
	allowed    bool
	reason     string
	retryAfter time.Duration
	raw        *redisstate.AuthQuotaReservation
}

func NewQuotaLimiter(store *redisstate.Store) cliproxyauth.QuotaLimiter {
	if store == nil {
		return nil
	}
	return &quotaLimiter{store: store}
}

func (l *quotaLimiter) Reserve(ctx context.Context, auth *cliproxyauth.Auth) (cliproxyauth.QuotaReservation, error) {
	if l == nil || l.store == nil || auth == nil {
		return nil, nil
	}
	reservation, err := l.store.ReserveAuthQuota(ctx, auth.ID, auth.Attributes)
	if err != nil {
		return nil, err
	}
	if reservation == nil {
		return nil, nil
	}
	return &quotaReservation{
		allowed:    reservation.Allowed,
		reason:     reservation.Reason,
		retryAfter: reservation.RetryAfter,
		raw:        reservation,
	}, nil
}

func (r *quotaReservation) Allowed() bool {
	if r == nil {
		return true
	}
	return r.allowed
}

func (r *quotaReservation) Reason() string {
	if r == nil {
		return ""
	}
	return r.reason
}

func (r *quotaReservation) RetryAfter() time.Duration {
	if r == nil {
		return 0
	}
	return r.retryAfter
}

func (r *quotaReservation) Release(ctx context.Context) {
	if r == nil || r.raw == nil {
		return
	}
	r.raw.Release(ctx)
}

type stateHook struct {
	store *redisstate.Store
}

func NewHook(store *redisstate.Store) cliproxyauth.Hook {
	if store == nil {
		return cliproxyauth.NoopHook{}
	}
	return &stateHook{store: store}
}

func (h *stateHook) OnAuthRegistered(ctx context.Context, auth *cliproxyauth.Auth) {
	h.sync(ctx, auth)
}

func (h *stateHook) OnAuthUpdated(ctx context.Context, auth *cliproxyauth.Auth) {
	h.sync(ctx, auth)
}

func (h *stateHook) OnResult(context.Context, cliproxyauth.Result) {}

func (h *stateHook) sync(ctx context.Context, auth *cliproxyauth.Auth) {
	if h == nil || h.store == nil || auth == nil {
		return
	}
	status := string(auth.Status)
	reason := auth.StatusMessage
	if auth.Disabled || auth.Status == cliproxyauth.StatusDisabled {
		status = "disabled"
	}
	if auth.Quota.Exceeded {
		status = "cooldown"
		if auth.Quota.Reason != "" {
			reason = auth.Quota.Reason
		}
	}
	if auth.Unavailable && status == string(cliproxyauth.StatusError) {
		status = "rate_limited"
	}
	_ = h.store.SetAuthStatus(ctx, auth.ID, status, reason, auth.NextRetryAfter)
}

type usagePlugin struct {
	store *redisstate.Store
}

func NewUsagePlugin(store *redisstate.Store) coreusage.Plugin {
	if store == nil {
		return nil
	}
	return &usagePlugin{store: store}
}

func (p *usagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.store == nil {
		return
	}
	_ = p.store.RecordUsage(ctx, record)
}
