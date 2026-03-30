package requestctx

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

const (
	HeaderTenantID     = "X-CPA-Tenant-ID"
	HeaderRequestID    = "X-CPA-Request-ID"
	HeaderProxyPool    = "X-CPA-Proxy-Pool"
	HeaderRetryAttempt = "X-CPA-Retry-Attempt"
)

const (
	MetadataTenantID     = "tenant_id"
	MetadataRequestID    = "request_id"
	MetadataProxyPool    = "proxy_pool"
	MetadataRetryAttempt = "retry_attempt"
)

type metadataContextKey struct{}

// WithMetadata attaches execution metadata to the context.
func WithMetadata(ctx context.Context, metadata map[string]any) context.Context {
	if len(metadata) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, metadataContextKey{}, cloneMetadata(metadata))
}

// MetadataFromContext returns execution metadata from the context when available.
func MetadataFromContext(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}
	if value, ok := ctx.Value(metadataContextKey{}).(map[string]any); ok && len(value) > 0 {
		return cloneMetadata(value)
	}
	return nil
}

// ApplyHeaders writes internal metadata headers to the request.
func ApplyHeaders(header http.Header, metadata map[string]any) {
	if header == nil || len(metadata) == 0 {
		return
	}
	if tenantID := MetadataString(metadata, MetadataTenantID); tenantID != "" {
		header.Set(HeaderTenantID, tenantID)
	}
	if requestID := MetadataString(metadata, MetadataRequestID); requestID != "" {
		header.Set(HeaderRequestID, requestID)
	}
	if proxyPool := MetadataString(metadata, MetadataProxyPool); proxyPool != "" {
		header.Set(HeaderProxyPool, proxyPool)
	}
	if retryAttempt := MetadataString(metadata, MetadataRetryAttempt); retryAttempt != "" {
		header.Set(HeaderRetryAttempt, retryAttempt)
	}
}

// MetadataFromHeaders extracts trusted internal metadata headers from the request.
func MetadataFromHeaders(header http.Header) map[string]any {
	if header == nil {
		return nil
	}
	metadata := map[string]any{}
	if tenantID := strings.TrimSpace(header.Get(HeaderTenantID)); tenantID != "" {
		metadata[MetadataTenantID] = tenantID
	}
	if requestID := strings.TrimSpace(header.Get(HeaderRequestID)); requestID != "" {
		metadata[MetadataRequestID] = requestID
	}
	if proxyPool := strings.TrimSpace(header.Get(HeaderProxyPool)); proxyPool != "" {
		metadata[MetadataProxyPool] = proxyPool
	}
	if retryAttempt := strings.TrimSpace(header.Get(HeaderRetryAttempt)); retryAttempt != "" {
		metadata[MetadataRetryAttempt] = retryAttempt
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

// MetadataString returns the string form of a metadata value.
func MetadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", value))
	}
}

func cloneMetadata(metadata map[string]any) map[string]any {
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}
