package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics collects and exposes server metrics
type Metrics struct {
	// Counters
	totalRequests    atomic.Int64
	totalConnections atomic.Int64
	bytesIn          atomic.Int64
	bytesOut         atomic.Int64

	// Per-subdomain metrics
	subdomainMetrics sync.Map // map[string]*SubdomainMetrics
}

// SubdomainMetrics holds metrics for a specific subdomain
type SubdomainMetrics struct {
	Requests    atomic.Int64
	BytesIn     atomic.Int64
	BytesOut    atomic.Int64
	Connections atomic.Int64
}

// NewMetrics creates a new metrics collector
func NewMetrics() *Metrics {
	return &Metrics{}
}

// RecordRequest increments the request counter for a subdomain
func (m *Metrics) RecordRequest(subdomain string) {
	m.totalRequests.Add(1)

	sm := m.getOrCreateSubdomainMetrics(subdomain)
	sm.Requests.Add(1)
}

// RecordConnection records a new connection for a subdomain
func (m *Metrics) RecordConnection(subdomain string) {
	m.totalConnections.Add(1)

	sm := m.getOrCreateSubdomainMetrics(subdomain)
	sm.Connections.Add(1)
}

// RecordBytes records bytes transferred
func (m *Metrics) RecordBytes(subdomain string, in, out int64) {
	m.bytesIn.Add(in)
	m.bytesOut.Add(out)

	sm := m.getOrCreateSubdomainMetrics(subdomain)
	sm.BytesIn.Add(in)
	sm.BytesOut.Add(out)
}

func (m *Metrics) getOrCreateSubdomainMetrics(subdomain string) *SubdomainMetrics {
	if val, ok := m.subdomainMetrics.Load(subdomain); ok {
		return val.(*SubdomainMetrics)
	}

	sm := &SubdomainMetrics{}
	actual, _ := m.subdomainMetrics.LoadOrStore(subdomain, sm)
	return actual.(*SubdomainMetrics)
}

// MetricsResponse represents the JSON response for metrics
type MetricsResponse struct {
	TotalRequests    int64                     `json:"total_requests"`
	TotalConnections int64                     `json:"total_connections"`
	BytesIn          int64                     `json:"bytes_in"`
	BytesOut         int64                     `json:"bytes_out"`
	Subdomains       map[string]SubdomainStats `json:"subdomains"`
}

// SubdomainStats represents stats for a single subdomain
type SubdomainStats struct {
	Requests    int64 `json:"requests"`
	Connections int64 `json:"connections"`
	BytesIn     int64 `json:"bytes_in"`
	BytesOut    int64 `json:"bytes_out"`
}

// GetStats returns current metrics as a response struct
func (m *Metrics) GetStats() MetricsResponse {
	resp := MetricsResponse{
		TotalRequests:    m.totalRequests.Load(),
		TotalConnections: m.totalConnections.Load(),
		BytesIn:          m.bytesIn.Load(),
		BytesOut:         m.bytesOut.Load(),
		Subdomains:       make(map[string]SubdomainStats),
	}

	m.subdomainMetrics.Range(func(key, value interface{}) bool {
		subdomain := key.(string)
		sm := value.(*SubdomainMetrics)
		resp.Subdomains[subdomain] = SubdomainStats{
			Requests:    sm.Requests.Load(),
			Connections: sm.Connections.Load(),
			BytesIn:     sm.BytesIn.Load(),
			BytesOut:    sm.BytesOut.Load(),
		}
		return true
	})

	return resp
}

// ServeMetrics starts an HTTP server for metrics
func (m *Metrics) ServeMetrics(ctx context.Context, port int, logger interface{ Info(msg string, args ...any) }) {
	mux := http.NewServeMux()

	// JSON metrics endpoint
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.GetStats())
	})

	// Prometheus-compatible endpoint (text format)
	mux.HandleFunc("/metrics/prometheus", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		m.writePrometheusMetrics(w)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/metrics", http.StatusFound)
	})

	addr := fmt.Sprintf(":%d", port)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	logger.Info("metrics endpoint listening", "addr", addr)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// Log error but don't crash
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdownCtx)
}

// writePrometheusMetrics writes metrics in Prometheus text format
func (m *Metrics) writePrometheusMetrics(w http.ResponseWriter) {
	fmt.Fprintf(w, "# HELP tunnel_requests_total Total number of requests\n")
	fmt.Fprintf(w, "# TYPE tunnel_requests_total counter\n")
	fmt.Fprintf(w, "tunnel_requests_total %d\n\n", m.totalRequests.Load())

	fmt.Fprintf(w, "# HELP tunnel_connections_total Total number of connections\n")
	fmt.Fprintf(w, "# TYPE tunnel_connections_total counter\n")
	fmt.Fprintf(w, "tunnel_connections_total %d\n\n", m.totalConnections.Load())

	fmt.Fprintf(w, "# HELP tunnel_bytes_in_total Total bytes received\n")
	fmt.Fprintf(w, "# TYPE tunnel_bytes_in_total counter\n")
	fmt.Fprintf(w, "tunnel_bytes_in_total %d\n\n", m.bytesIn.Load())

	fmt.Fprintf(w, "# HELP tunnel_bytes_out_total Total bytes sent\n")
	fmt.Fprintf(w, "# TYPE tunnel_bytes_out_total counter\n")
	fmt.Fprintf(w, "tunnel_bytes_out_total %d\n\n", m.bytesOut.Load())

	// Per-subdomain metrics
	fmt.Fprintf(w, "# HELP tunnel_subdomain_requests_total Requests per subdomain\n")
	fmt.Fprintf(w, "# TYPE tunnel_subdomain_requests_total counter\n")
	m.subdomainMetrics.Range(func(key, value interface{}) bool {
		subdomain := key.(string)
		sm := value.(*SubdomainMetrics)
		fmt.Fprintf(w, "tunnel_subdomain_requests_total{subdomain=\"%s\"} %d\n", subdomain, sm.Requests.Load())
		return true
	})
	fmt.Fprintln(w)

	fmt.Fprintf(w, "# HELP tunnel_subdomain_bytes_in_total Bytes received per subdomain\n")
	fmt.Fprintf(w, "# TYPE tunnel_subdomain_bytes_in_total counter\n")
	m.subdomainMetrics.Range(func(key, value interface{}) bool {
		subdomain := key.(string)
		sm := value.(*SubdomainMetrics)
		fmt.Fprintf(w, "tunnel_subdomain_bytes_in_total{subdomain=\"%s\"} %d\n", subdomain, sm.BytesIn.Load())
		return true
	})
	fmt.Fprintln(w)

	fmt.Fprintf(w, "# HELP tunnel_subdomain_bytes_out_total Bytes sent per subdomain\n")
	fmt.Fprintf(w, "# TYPE tunnel_subdomain_bytes_out_total counter\n")
	m.subdomainMetrics.Range(func(key, value interface{}) bool {
		subdomain := key.(string)
		sm := value.(*SubdomainMetrics)
		fmt.Fprintf(w, "tunnel_subdomain_bytes_out_total{subdomain=\"%s\"} %d\n", subdomain, sm.BytesOut.Load())
		return true
	})
}
