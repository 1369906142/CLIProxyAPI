package gateway

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Metrics struct {
	requestsTotal atomic.Int64
	successTotal  atomic.Int64
	failureTotal  atomic.Int64
	retriesTotal  atomic.Int64
	inflight      atomic.Int64

	mu            sync.Mutex
	statusCounts  map[string]int64
	tenantCounts  map[string]int64
	secondBuckets map[int64]int64
}

func NewMetrics() *Metrics {
	return &Metrics{
		statusCounts:  make(map[string]int64),
		tenantCounts:  make(map[string]int64),
		secondBuckets: make(map[int64]int64),
	}
}

func (m *Metrics) Begin() func() {
	if m == nil {
		return func() {}
	}
	m.inflight.Add(1)
	return func() {
		m.inflight.Add(-1)
	}
}

func (m *Metrics) Record(tenantID string, statusCode int, retries int, ok bool) {
	if m == nil {
		return
	}
	m.requestsTotal.Add(1)
	if ok {
		m.successTotal.Add(1)
	} else {
		m.failureTotal.Add(1)
	}
	if retries > 0 {
		m.retriesTotal.Add(int64(retries))
	}
	now := time.Now().Unix()
	statusLabel := fmt.Sprintf("%dxx", statusCode/100)
	if statusCode <= 0 {
		statusLabel = "error"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusCounts[statusLabel]++
	if tenantID != "" {
		m.tenantCounts[tenantID]++
	}
	m.secondBuckets[now]++
	cutoff := now - 60
	for second := range m.secondBuckets {
		if second < cutoff {
			delete(m.secondBuckets, second)
		}
	}
}

func (m *Metrics) Prometheus() string {
	if m == nil {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("# TYPE gateway_requests_total counter\n")
	builder.WriteString(fmt.Sprintf("gateway_requests_total %d\n", m.requestsTotal.Load()))
	builder.WriteString("# TYPE gateway_requests_success_total counter\n")
	builder.WriteString(fmt.Sprintf("gateway_requests_success_total %d\n", m.successTotal.Load()))
	builder.WriteString("# TYPE gateway_requests_failure_total counter\n")
	builder.WriteString(fmt.Sprintf("gateway_requests_failure_total %d\n", m.failureTotal.Load()))
	builder.WriteString("# TYPE gateway_retries_total counter\n")
	builder.WriteString(fmt.Sprintf("gateway_retries_total %d\n", m.retriesTotal.Load()))
	builder.WriteString("# TYPE gateway_inflight_requests gauge\n")
	builder.WriteString(fmt.Sprintf("gateway_inflight_requests %d\n", m.inflight.Load()))

	m.mu.Lock()
	defer m.mu.Unlock()
	var qpsWindow int64
	for _, count := range m.secondBuckets {
		qpsWindow += count
	}
	builder.WriteString("# TYPE gateway_qps_rolling gauge\n")
	builder.WriteString(fmt.Sprintf("gateway_qps_rolling %.4f\n", float64(qpsWindow)/60.0))
	for status, count := range m.statusCounts {
		builder.WriteString(fmt.Sprintf("gateway_status_total{code=%q} %d\n", status, count))
	}
	for tenantID, count := range m.tenantCounts {
		builder.WriteString(fmt.Sprintf("gateway_tenant_requests_total{tenant=%q} %d\n", tenantID, count))
	}
	return builder.String()
}
