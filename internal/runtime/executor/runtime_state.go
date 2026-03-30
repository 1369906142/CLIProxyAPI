package executor

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/redisstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/requestctx"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

type transportRuntimeState struct {
	mu         sync.RWMutex
	cfg        *config.Config
	store      *redisstate.Store
	transports map[string]*http.Transport
}

var transportRuntime = &transportRuntimeState{
	transports: make(map[string]*http.Transport),
}

func ConfigureTransportRuntime(cfg *config.Config, store *redisstate.Store) {
	transportRuntime.mu.Lock()
	defer transportRuntime.mu.Unlock()
	transportRuntime.cfg = cfg
	transportRuntime.store = store
	if transportRuntime.transports == nil {
		transportRuntime.transports = make(map[string]*http.Transport)
	}
}

func currentTransportStore() *redisstate.Store {
	transportRuntime.mu.RLock()
	defer transportRuntime.mu.RUnlock()
	return transportRuntime.store
}

func transportForProxy(proxyURL string) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	transportRuntime.mu.RLock()
	if transportRuntime.transports != nil {
		if cached := transportRuntime.transports[proxyURL]; cached != nil {
			transportRuntime.mu.RUnlock()
			return cached.Clone()
		}
	}
	transportRuntime.mu.RUnlock()

	var transport *http.Transport
	if proxyURL == "" {
		transport = proxyutil.NewDirectTransport()
	} else {
		built, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
		if errBuild != nil {
			log.Errorf("%v", errBuild)
			return nil
		}
		transport = built
	}
	if transport == nil {
		return nil
	}

	transportRuntime.mu.Lock()
	defer transportRuntime.mu.Unlock()
	if transportRuntime.transports == nil {
		transportRuntime.transports = make(map[string]*http.Transport)
	}
	transportRuntime.transports[proxyURL] = transport
	return transport.Clone()
}

func selectProxyCandidate(ctx context.Context, cfg *config.Config) (redisstate.ProxyCandidate, bool) {
	store := currentTransportStore()
	if store == nil || cfg == nil {
		return redisstate.ProxyCandidate{}, false
	}
	metadata := requestctx.MetadataFromContext(ctx)
	poolID := requestctx.MetadataString(metadata, requestctx.MetadataProxyPool)
	if poolID == "" {
		return redisstate.ProxyCandidate{}, false
	}
	candidates := proxyCandidates(cfg, poolID)
	if len(candidates) == 0 {
		return redisstate.ProxyCandidate{}, false
	}
	candidate, ok, err := store.SelectProxy(ctx, poolID, candidates)
	if err != nil {
		log.WithError(err).Warnf("proxy pool select failed for %s", poolID)
		return redisstate.ProxyCandidate{}, false
	}
	if !ok {
		return redisstate.ProxyCandidate{}, false
	}
	return candidate, true
}

func proxyCandidates(cfg *config.Config, poolID string) []redisstate.ProxyCandidate {
	if cfg == nil || poolID == "" {
		return nil
	}
	poolID = strings.ToLower(strings.TrimSpace(poolID))
	candidates := make([]redisstate.ProxyCandidate, 0)
	for _, entry := range cfg.Transport.ProxyPools {
		if strings.ToLower(strings.TrimSpace(entry.ID)) != poolID {
			continue
		}
		urls := make([]string, 0, 1+len(entry.URLs))
		if trimmed := strings.TrimSpace(entry.URL); trimmed != "" {
			urls = append(urls, trimmed)
		}
		for _, raw := range entry.URLs {
			if trimmed := strings.TrimSpace(raw); trimmed != "" {
				urls = append(urls, trimmed)
			}
		}
		for _, raw := range urls {
			candidates = append(candidates, redisstate.ProxyCandidate{
				PoolID:      poolID,
				URL:         raw,
				Weight:      entry.Weight,
				MaxFailures: entry.MaxFailures,
				Cooldown:    timeDurationSeconds(entry.CooldownSeconds),
				Mode:        strings.TrimSpace(entry.Mode),
			})
		}
	}
	return candidates
}

func timeDurationSeconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

type proxyHealthRoundTripper struct {
	base      http.RoundTripper
	store     *redisstate.Store
	candidate redisstate.ProxyCandidate
}

func (t *proxyHealthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	success := err == nil
	if success && resp != nil {
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			success = false
		}
	}
	if t.store != nil {
		if markErr := t.store.MarkProxyResult(req.Context(), t.candidate, success); markErr != nil {
			log.WithError(markErr).Debug("mark proxy result failed")
		}
	}
	return resp, err
}
