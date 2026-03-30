package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/redisstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/requestctx"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var backoffSchedule = []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 900 * time.Millisecond}

type Server struct {
	cfg       *Config
	store     *redisstate.Store
	metrics   *Metrics
	backend   *url.URL
	tenantKey map[string]TenantConfig
	client    *http.Client
}

func NewServer(cfg *Config, store *redisstate.Store) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("gateway config is nil")
	}
	backendURL, err := url.Parse(cfg.Backend.URL)
	if err != nil {
		return nil, err
	}
	tenantKey := make(map[string]TenantConfig, len(cfg.Tenants))
	for _, tenant := range cfg.Tenants {
		tenantKey[tenant.APIKey] = tenant
	}
	transport := newBackendTransport(cfg.Timeouts)
	return &Server{
		cfg:       cfg,
		store:     store,
		metrics:   NewMetrics(),
		backend:   backendURL,
		tenantKey: tenantKey,
		client:    &http.Client{Transport: transport},
	}, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	case "/readyz":
		if s.store == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "redis_unavailable"})
			return
		}
		if err := s.store.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "redis_error", "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
		return
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, s.metrics.Prometheus())
		return
	}
	s.handleProxy(w, r)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	done := s.metrics.Begin()
	defer done()

	tenant, ok := s.authenticate(r)
	if !ok {
		s.metrics.Record("", http.StatusUnauthorized, 0, false)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	if s.store == nil {
		s.metrics.Record(tenant.ID, http.StatusServiceUnavailable, 0, false)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "redis unavailable"})
		return
	}

	if decision, err := s.store.RateLimit(r.Context(), "tenant:"+tenant.ID, tenant.QPS, tenant.Burst); err != nil {
		log.WithError(err).Error("gateway rate limit failed")
		s.metrics.Record(tenant.ID, http.StatusServiceUnavailable, 0, false)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "rate limiter unavailable"})
		return
	} else if !decision.Allowed {
		if decision.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(decision.RetryAfter.Seconds())))
		}
		s.metrics.Record(tenant.ID, http.StatusTooManyRequests, 0, false)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limit exceeded"})
		return
	}

	requestID := strings.TrimSpace(r.Header.Get(requestctx.HeaderRequestID))
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.metrics.Record(tenant.ID, http.StatusBadRequest, 0, false)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	modelName := extractModelName(body)
	streaming := isStreamingRequest(r, body)

	statusCode, retries, proxyErr := s.forwardRequest(w, r, tenant, requestID, modelName, body, streaming)
	s.metrics.Record(tenant.ID, statusCode, retries, proxyErr == nil && statusCode < http.StatusBadRequest)

	fields := log.Fields{
		"tenant_id":    tenant.ID,
		"request_id":   requestID,
		"path":         r.URL.Path,
		"method":       r.Method,
		"model":        modelName,
		"status_code":  statusCode,
		"retry_count":  retries,
		"backend_host": s.backend.Host,
		"stream":       streaming,
	}
	entry := log.WithFields(fields)
	if proxyErr != nil {
		if responseCommitted(proxyErr) {
			entry.WithError(proxyErr).Info("gateway upstream stream closed after response commit")
			return
		}
		entry.WithError(proxyErr).Warn("gateway upstream request failed")
		return
	}
	entry.Info("gateway request completed")
}

func (s *Server) forwardRequest(w http.ResponseWriter, r *http.Request, tenant TenantConfig, requestID, modelName string, body []byte, streaming bool) (int, int, error) {
	totalTimeout := time.Duration(tenant.TimeoutMS) * time.Millisecond
	totalCtx, cancel := context.WithTimeout(r.Context(), totalTimeout)
	defer cancel()

	attempts := tenant.Retry.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		statusCode, retryable, err := s.doAttempt(totalCtx, w, r, tenant, requestID, modelName, body, streaming, attempt)
		if err == nil {
			return statusCode, attempt - 1, nil
		}
		lastErr = err
		if responseCommitted(err) {
			return statusCode, attempt - 1, err
		}
		if !retryable || attempt == attempts {
			if statusCode == 0 {
				statusCode = http.StatusBadGateway
			}
			writeJSON(w, statusCode, map[string]any{"error": err.Error(), "request_id": requestID})
			return statusCode, attempt - 1, err
		}
		if wait := jitterBackoff(attempt - 1); wait > 0 {
			select {
			case <-time.After(wait):
			case <-totalCtx.Done():
				writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": totalCtx.Err().Error(), "request_id": requestID})
				return http.StatusGatewayTimeout, attempt - 1, totalCtx.Err()
			}
		}
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": lastErr.Error(), "request_id": requestID})
	return http.StatusBadGateway, attempts - 1, lastErr
}

func (s *Server) doAttempt(ctx context.Context, w http.ResponseWriter, r *http.Request, tenant TenantConfig, requestID, modelName string, body []byte, streaming bool, attempt int) (int, bool, error) {
	outboundURL := *s.backend
	outboundURL.Path = strings.TrimRight(s.backend.Path, "/") + r.URL.Path
	outboundURL.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(ctx, r.Method, outboundURL.String(), bytes.NewReader(body))
	if err != nil {
		return http.StatusBadGateway, false, err
	}
	copyRequestHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+s.cfg.Backend.InternalAPIKey)
	metadata := map[string]any{
		requestctx.MetadataTenantID:     tenant.ID,
		requestctx.MetadataRequestID:    requestID,
		requestctx.MetadataRetryAttempt: strconv.Itoa(attempt),
	}
	if tenant.ProxyPool != "" {
		metadata[requestctx.MetadataProxyPool] = tenant.ProxyPool
	}
	requestctx.ApplyHeaders(req.Header, metadata)
	req.Host = s.backend.Host

	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return http.StatusGatewayTimeout, false, err
		}
		return http.StatusBadGateway, true, err
	}
	defer resp.Body.Close()

	if streaming {
		return s.writeStreamingResponse(ctx, w, resp)
	}
	payload, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return http.StatusBadGateway, true, readErr
	}
	if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, true, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(payload)
	return resp.StatusCode, false, nil
}

func (s *Server) writeStreamingResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response) (int, bool, error) {
	if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, true, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return http.StatusInternalServerError, false, fmt.Errorf("streaming not supported by response writer")
	}

	firstChunk := make([]byte, 32*1024)
	n, readErr := resp.Body.Read(firstChunk)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return http.StatusBadGateway, true, readErr
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if n > 0 {
		if _, err := w.Write(firstChunk[:n]); err != nil {
			return http.StatusBadGateway, false, err
		}
		flusher.Flush()
	}
	if errors.Is(readErr, io.EOF) {
		return resp.StatusCode, false, nil
	}
	idleTimeout := time.Duration(s.cfg.Timeouts.StreamIdleMS) * time.Millisecond
	if err := streamCopyWithIdle(ctx, resp.Body, w, flusher, idleTimeout); err != nil {
		return resp.StatusCode, false, &responseCommittedError{cause: err}
	}
	return resp.StatusCode, false, nil
}

func newBackendTransport(timeouts TimeoutConfig) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   time.Duration(timeouts.ConnectMS) * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: time.Duration(timeouts.ResponseHeaderMS) * time.Millisecond,
	}
}

func (s *Server) authenticate(r *http.Request) (TenantConfig, bool) {
	apiKey := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if apiKey == "" {
		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			apiKey = strings.TrimSpace(authHeader[7:])
		}
	}
	if apiKey == "" {
		return TenantConfig{}, false
	}
	tenant, ok := s.tenantKey[apiKey]
	return tenant, ok
}

func extractModelName(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(body, "model").String())
}

func isStreamingRequest(r *http.Request, body []byte) bool {
	if strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Accept"))), "text/event-stream") {
		return true
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	return gjson.GetBytes(body, "stream").Bool()
}

func jitterBackoff(index int) time.Duration {
	if index < 0 {
		index = 0
	}
	if index >= len(backoffSchedule) {
		index = len(backoffSchedule) - 1
	}
	base := backoffSchedule[index]
	jitter := time.Duration(float64(base) * 0.2)
	if jitter <= 0 {
		return base
	}
	return base - jitter + time.Duration(randInt64(int64(2*jitter)))
}

func randInt64(max int64) int64 {
	if max <= 0 {
		return 0
	}
	return time.Now().UnixNano() % max
}

type responseCommittedError struct {
	cause error
}

func (e *responseCommittedError) Error() string {
	if e == nil || e.cause == nil {
		return "response already committed"
	}
	return e.cause.Error()
}

func (e *responseCommittedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func responseCommitted(err error) bool {
	var committedErr *responseCommittedError
	return errors.As(err, &committedErr)
}

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		switch canonical {
		case "Authorization", "Proxy-Authorization", "Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "X-Api-Key", "X-Cpa-Tenant-Id", "X-Cpa-Request-Id", "X-Cpa-Proxy-Pool", "X-Cpa-Retry-Attempt":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		switch canonical {
		case "Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func streamCopyWithIdle(ctx context.Context, reader io.Reader, writer io.Writer, flusher http.Flusher, idleTimeout time.Duration) error {
	if idleTimeout <= 0 {
		_, err := io.Copy(writer, reader)
		return err
	}
	type readResult struct {
		n   int
		err error
		buf []byte
	}
	for {
		buf := make([]byte, 32*1024)
		resultCh := make(chan readResult, 1)
		go func() {
			n, err := reader.Read(buf)
			resultCh <- readResult{n: n, err: err, buf: buf}
		}()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(idleTimeout):
			return fmt.Errorf("stream idle timeout")
		case result := <-resultCh:
			if result.n > 0 {
				if _, err := writer.Write(result.buf[:result.n]); err != nil {
					return err
				}
				flusher.Flush()
			}
			if errors.Is(result.err, io.EOF) {
				return nil
			}
			if result.err != nil {
				return result.err
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	data, err := json.Marshal(payload)
	if err != nil {
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
		return
	}
	_, _ = w.Write(data)
}
