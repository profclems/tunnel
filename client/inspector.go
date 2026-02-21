package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RequestRecord holds details of a captured request
type RequestRecord struct {
	ID        string      `json:"id"`
	Method    string      `json:"method"`
	Path      string      `json:"path"`
	Host      string      `json:"host"` // Original Host header for replay
	URL       string      `json:"url"`  // Full URL for replay
	Status    int         `json:"status"`
	Duration  string      `json:"duration"`
	Timestamp time.Time   `json:"timestamp"`
	ReqHeader http.Header `json:"req_header"`
	ResHeader http.Header `json:"res_header"`
	ReqBody   string      `json:"req_body"`
	ResBody   string      `json:"res_body"`

	// Enhanced metrics fields
	DurationMs  int64  `json:"duration_ms"`   // Duration in milliseconds for graphing
	ReqBodySize int64  `json:"req_body_size"` // Request body size in bytes
	ResBodySize int64  `json:"res_body_size"` // Response body size in bytes
	ContentType string `json:"content_type"`  // Response content type for syntax highlighting
	TunnelID    string `json:"tunnel_id"`     // Which tunnel this request came through
}

// LatencyPoint represents a single latency measurement for graphing
type LatencyPoint struct {
	Timestamp time.Time `json:"ts"`
	LatencyMs int64     `json:"ms"`
	TunnelID  string    `json:"tunnel_id,omitempty"`
}

// InspectorMetrics tracks aggregate metrics for the dashboard
type InspectorMetrics struct {
	mu sync.RWMutex

	// Rolling time-series (last 60 points)
	latencyHistory []LatencyPoint
	rateHistory    []int // requests per second for last 60 seconds
	rateBuckets    map[int64]int

	// Totals
	TotalBytesIn  int64 `json:"total_bytes_in"`
	TotalBytesOut int64 `json:"total_bytes_out"`
	TotalRequests int64 `json:"total_requests"`
	ErrorCount    int64 `json:"error_count"`

	// Last update
	LastRequestTime time.Time `json:"last_request_time"`
}

// MetricsSnapshot is the JSON response for /api/metrics
type MetricsSnapshot struct {
	LatencyHistory []LatencyPoint `json:"latency_history"`
	RateHistory    []int          `json:"rate_history"`
	TotalBytesIn   int64          `json:"total_bytes_in"`
	TotalBytesOut  int64          `json:"total_bytes_out"`
	TotalRequests  int64          `json:"total_requests"`
	ErrorCount     int64          `json:"error_count"`
	AvgLatencyMs   int64          `json:"avg_latency_ms"`
	RequestsPerMin float64        `json:"requests_per_min"`
}

// Inspector manages request introspection
type Inspector struct {
	mu        sync.RWMutex
	requests  []*RequestRecord
	maxSize   int
	broadcast chan *RequestRecord

	// SSE client management
	sseClientsMu sync.RWMutex
	sseClients   map[chan *RequestRecord]struct{}

	// Local address for replay functionality
	localAddr string

	// Client reference for tunnel management (set via SetClient)
	client *Client

	// Config manager for persistence
	configMgr *ConfigManager

	// Metrics tracking
	metrics *InspectorMetrics
}

// SetClient sets the client reference for tunnel management
func (i *Inspector) SetClient(c *Client) {
	i.client = c
}

// SetConfigManager sets the config manager for persistence
func (i *Inspector) SetConfigManager(cm *ConfigManager) {
	i.configMgr = cm
}

func NewInspector() *Inspector {
	return &Inspector{
		requests:   make([]*RequestRecord, 0),
		maxSize:    100, // Keep last 100 requests
		broadcast:  make(chan *RequestRecord, 10),
		sseClients: make(map[chan *RequestRecord]struct{}),
		metrics: &InspectorMetrics{
			latencyHistory: make([]LatencyPoint, 0, 60),
			rateHistory:    make([]int, 60),
			rateBuckets:    make(map[int64]int),
		},
	}
}

// addSSEClient registers a new SSE client channel
func (i *Inspector) addSSEClient(ch chan *RequestRecord) {
	i.sseClientsMu.Lock()
	defer i.sseClientsMu.Unlock()
	i.sseClients[ch] = struct{}{}
}

// removeSSEClient unregisters an SSE client channel
func (i *Inspector) removeSSEClient(ch chan *RequestRecord) {
	i.sseClientsMu.Lock()
	defer i.sseClientsMu.Unlock()
	delete(i.sseClients, ch)
	close(ch)
}

// runSSEBroadcaster distributes new requests to all connected SSE clients
func (i *Inspector) runSSEBroadcaster(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-i.broadcast:
			i.sseClientsMu.RLock()
			for clientCh := range i.sseClients {
				select {
				case clientCh <- rec:
				default:
					// Client channel full, skip
				}
			}
			i.sseClientsMu.RUnlock()
		}
	}
}

func (i *Inspector) addRecord(rec *RequestRecord) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Prepend
	i.requests = append([]*RequestRecord{rec}, i.requests...)
	if len(i.requests) > i.maxSize {
		i.requests = i.requests[:i.maxSize]
	}
}

func (i *Inspector) GetRecent() []*RequestRecord {
	i.mu.RLock()
	defer i.mu.RUnlock()
	// Return copy
	dst := make([]*RequestRecord, len(i.requests))
	copy(dst, i.requests)
	return dst
}

// GetRequestByID retrieves a specific request record by ID
func (i *Inspector) GetRequestByID(id string) *RequestRecord {
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, rec := range i.requests {
		if rec.ID == id {
			return rec
		}
	}
	return nil
}

// SetLocalAddr sets the local address for replay functionality
func (i *Inspector) SetLocalAddr(addr string) {
	i.localAddr = addr
}

// recordMetrics records metrics for a completed request
func (i *Inspector) recordMetrics(rec *RequestRecord) {
	i.metrics.mu.Lock()
	defer i.metrics.mu.Unlock()

	// Update totals
	i.metrics.TotalRequests++
	i.metrics.TotalBytesIn += rec.ReqBodySize
	i.metrics.TotalBytesOut += rec.ResBodySize
	i.metrics.LastRequestTime = rec.Timestamp

	if rec.Status >= 500 {
		i.metrics.ErrorCount++
	}

	// Add latency point
	point := LatencyPoint{
		Timestamp: rec.Timestamp,
		LatencyMs: rec.DurationMs,
		TunnelID:  rec.TunnelID,
	}
	i.metrics.latencyHistory = append(i.metrics.latencyHistory, point)
	if len(i.metrics.latencyHistory) > 60 {
		i.metrics.latencyHistory = i.metrics.latencyHistory[1:]
	}

	// Update rate tracking
	sec := rec.Timestamp.Unix()
	i.metrics.rateBuckets[sec]++

	// Cleanup old buckets (keep last 60 seconds)
	cutoff := sec - 60
	for k := range i.metrics.rateBuckets {
		if k < cutoff {
			delete(i.metrics.rateBuckets, k)
		}
	}
}

// GetMetricsSnapshot returns a snapshot of current metrics
func (i *Inspector) GetMetricsSnapshot() MetricsSnapshot {
	i.metrics.mu.RLock()
	defer i.metrics.mu.RUnlock()

	// Copy latency history
	latency := make([]LatencyPoint, len(i.metrics.latencyHistory))
	copy(latency, i.metrics.latencyHistory)

	// Build rate history (last 60 seconds)
	now := time.Now().Unix()
	rateHistory := make([]int, 60)
	for j := 0; j < 60; j++ {
		sec := now - int64(59-j)
		rateHistory[j] = i.metrics.rateBuckets[sec]
	}

	// Calculate average latency
	var avgLatency int64
	if len(latency) > 0 {
		var total int64
		for _, p := range latency {
			total += p.LatencyMs
		}
		avgLatency = total / int64(len(latency))
	}

	// Calculate requests per minute
	var totalRate int
	for _, r := range rateHistory {
		totalRate += r
	}
	reqPerMin := float64(totalRate)

	return MetricsSnapshot{
		LatencyHistory: latency,
		RateHistory:    rateHistory,
		TotalBytesIn:   i.metrics.TotalBytesIn,
		TotalBytesOut:  i.metrics.TotalBytesOut,
		TotalRequests:  i.metrics.TotalRequests,
		ErrorCount:     i.metrics.ErrorCount,
		AvgLatencyMs:   avgLatency,
		RequestsPerMin: reqPerMin,
	}
}

// SearchRequests filters requests based on search parameters
func (i *Inspector) SearchRequests(method, path, bodySearch string, statusFrom, statusTo int) []*RequestRecord {
	i.mu.RLock()
	defer i.mu.RUnlock()

	var results []*RequestRecord
	for _, rec := range i.requests {
		// Filter by method
		if method != "" && rec.Method != method {
			continue
		}

		// Filter by path (substring match)
		if path != "" && !strings.Contains(rec.Path, path) {
			continue
		}

		// Filter by status range
		if statusFrom > 0 && rec.Status < statusFrom {
			continue
		}
		if statusTo > 0 && rec.Status > statusTo {
			continue
		}

		// Filter by body content (substring match)
		if bodySearch != "" {
			if !strings.Contains(rec.ReqBody, bodySearch) && !strings.Contains(rec.ResBody, bodySearch) {
				continue
			}
		}

		results = append(results, rec)
	}

	return results
}

// InspectHTTP handles an HTTP request with inspection, forwarding to local service
// and writing the response back to the stream. This is the proper HTTP-aware version
// that avoids the io.Copy goroutine race conditions.
func (i *Inspector) InspectHTTP(req *http.Request, stream net.Conn, localAddr string) {
	start := time.Now()
	id := uuid.NewString()

	// Capture Request Body (Limit to 1MB for recording)
	var reqBody string
	var reqBodyBytes []byte
	if req.Body != nil {
		limitReader := io.LimitReader(req.Body, 1024*1024)
		reqBodyBytes, _ = io.ReadAll(limitReader)
		req.Body = io.NopCloser(bytes.NewBuffer(reqBodyBytes)) // Restore for forwarding
		reqBody = string(reqBodyBytes)
	}

	// Build full URL for replay
	urlStr := req.URL.String()
	if req.URL.Host == "" && req.Host != "" {
		urlStr = "http://" + req.Host + req.URL.RequestURI()
	}

	// Extract subdomain/tunnel ID from host
	tunnelID := ""
	if parts := strings.Split(req.Host, "."); len(parts) > 0 {
		tunnelID = parts[0]
	}

	rec := &RequestRecord{
		ID:          id,
		Method:      req.Method,
		Path:        req.URL.Path,
		Host:        req.Host,
		URL:         urlStr,
		Timestamp:   start,
		ReqHeader:   req.Header.Clone(),
		ReqBody:     reqBody,
		ReqBodySize: int64(len(reqBodyBytes)),
		TunnelID:    tunnelID,
	}

	// Modify request for forwarding to local service
	req.URL.Scheme = "http"
	req.URL.Host = localAddr
	req.RequestURI = "" // Required for http.Client

	// Create HTTP client for forwarding
	transport := &http.Transport{
		DisableCompression: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Forward request to local service
	resp, err := client.Do(req)
	if err != nil {
		// Record error and send error response
		duration := time.Since(start)
		rec.Status = http.StatusBadGateway
		rec.Duration = duration.String()
		rec.DurationMs = duration.Milliseconds()
		i.addRecord(rec)
		i.recordMetrics(rec)

		errorResp := &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("Local service unavailable")),
		}
		errorResp.Header.Set("Content-Type", "text/plain")
		errorResp.Write(stream)
		return
	}
	defer resp.Body.Close()

	// Capture Response Body (Limit to 1MB for recording)
	// But we need to pass the full body through to the client
	var resBody string
	respBodyBytes, _ := io.ReadAll(resp.Body)
	if len(respBodyBytes) <= 1024*1024 {
		resBody = string(respBodyBytes)
	} else {
		resBody = string(respBodyBytes[:1024*1024]) + "... (truncated for display)"
	}

	// Restore body for writing to stream
	resp.Body = io.NopCloser(bytes.NewBuffer(respBodyBytes))
	resp.ContentLength = int64(len(respBodyBytes))

	duration := time.Since(start)
	rec.Status = resp.StatusCode
	rec.ResHeader = resp.Header.Clone()
	rec.ResBody = resBody
	rec.Duration = duration.String()
	rec.DurationMs = duration.Milliseconds()
	rec.ResBodySize = int64(len(respBodyBytes))
	rec.ContentType = resp.Header.Get("Content-Type")

	// Save Record
	i.addRecord(rec)

	// Record metrics
	i.recordMetrics(rec)

	// Broadcast to SSE clients
	select {
	case i.broadcast <- rec:
	default:
	}

	// Write response back to the stream
	resp.Write(stream)
}

// ReplayResult holds the result of a replayed request
type ReplayResult struct {
	ID        string      `json:"id"`
	Status    int         `json:"status"`
	Duration  string      `json:"duration"`
	ResHeader http.Header `json:"res_header"`
	ResBody   string      `json:"res_body"`
}

// headersToHAR converts http.Header to HAR format
func headersToHAR(h http.Header) []map[string]string {
	result := make([]map[string]string, 0)
	for name, values := range h {
		for _, value := range values {
			result = append(result, map[string]string{
				"name":  name,
				"value": value,
			})
		}
	}
	return result
}

// replayRequest replays a captured request to the local service
func (i *Inspector) replayRequest(rec *RequestRecord) (*ReplayResult, error) {
	start := time.Now()

	// Build the request to the local service
	reqBody := bytes.NewBufferString(rec.ReqBody)
	localURL := "http://" + i.localAddr + rec.Path

	req, err := http.NewRequest(rec.Method, localURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Copy original headers (except Host which is set by HTTP client)
	for key, values := range rec.ReqHeader {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}

	// Make the request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body (limit to 1MB)
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))

	result := &ReplayResult{
		ID:        uuid.NewString(),
		Status:    resp.StatusCode,
		Duration:  time.Since(start).String(),
		ResHeader: resp.Header.Clone(),
		ResBody:   string(bodyBytes),
	}

	return result, nil
}

// ServeDashboard starts the local web UI
func (i *Inspector) ServeDashboard(ctx context.Context, port int) {
	mux := http.NewServeMux()

	// Regular API endpoint for fetching all requests
	mux.HandleFunc("/api/requests", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		recs := i.GetRecent()
		jsonBytes, _ := json.Marshal(recs)
		w.Write(jsonBytes)
	})

	// Search/filter requests endpoint
	mux.HandleFunc("/api/requests/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		q := r.URL.Query()
		method := q.Get("method")
		path := q.Get("path")
		bodySearch := q.Get("q")

		var statusFrom, statusTo int
		if s := q.Get("status_from"); s != "" {
			fmt.Sscanf(s, "%d", &statusFrom)
		}
		if s := q.Get("status_to"); s != "" {
			fmt.Sscanf(s, "%d", &statusTo)
		}
		// Shorthand for status class
		if status := q.Get("status"); status != "" {
			switch status {
			case "2xx":
				statusFrom, statusTo = 200, 299
			case "4xx":
				statusFrom, statusTo = 400, 499
			case "5xx":
				statusFrom, statusTo = 500, 599
			}
		}

		results := i.SearchRequests(method, path, bodySearch, statusFrom, statusTo)
		jsonBytes, _ := json.Marshal(results)
		w.Write(jsonBytes)
	})

	// Metrics endpoint
	mux.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		snapshot := i.GetMetricsSnapshot()
		jsonBytes, _ := json.Marshal(snapshot)
		w.Write(jsonBytes)
	})

	// SSE endpoint for real-time updates
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "SSE not supported", http.StatusInternalServerError)
			return
		}

		// Create a channel to receive new requests
		clientChan := make(chan *RequestRecord, 10)

		// Register this client
		i.addSSEClient(clientChan)
		defer i.removeSSEClient(clientChan)

		// Send initial "connected" event
		fmt.Fprintf(w, "event: connected\ndata: {}\n\n")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			case rec := <-clientChan:
				jsonBytes, err := json.Marshal(rec)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: request\ndata: %s\n\n", jsonBytes)
				flusher.Flush()
			}
		}
	})

	// Replay endpoint - replay a captured request
	mux.HandleFunc("/api/replay/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Extract request ID from URL path
		id := strings.TrimPrefix(r.URL.Path, "/api/replay/")
		if id == "" {
			http.Error(w, "Request ID required", http.StatusBadRequest)
			return
		}

		// Find the request record
		rec := i.GetRequestByID(id)
		if rec == nil {
			http.Error(w, "Request not found", http.StatusNotFound)
			return
		}

		// Check if we have a local address configured
		if i.localAddr == "" {
			http.Error(w, "Replay not available - local address not configured", http.StatusServiceUnavailable)
			return
		}

		// Replay the request to the local service
		result, err := i.replayRequest(rec)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(result)
	})

	// Tunnel management API endpoints
	mux.HandleFunc("/api/tunnels", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if i.client == nil {
			json.NewEncoder(w).Encode([]TunnelConfig{})
			return
		}

		tunnels := i.client.GetTunnels()
		json.NewEncoder(w).Encode(tunnels)
	})

	mux.HandleFunc("/api/tunnels/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if i.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not connected to server"})
			return
		}

		var req struct {
			Type       string `json:"type"`
			Subdomain  string `json:"subdomain,omitempty"`
			RemotePort int    `json:"remote_port,omitempty"`
			LocalAddr  string `json:"local_addr"`
			Persist    bool   `json:"persist"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
			return
		}

		// Validate input
		if req.Type != "http" && req.Type != "tcp" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Type must be 'http' or 'tcp'"})
			return
		}
		if req.LocalAddr == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Local address is required"})
			return
		}

		// Validate local address format
		if _, _, err := net.SplitHostPort(req.LocalAddr); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid local address format (expected host:port)"})
			return
		}

		tunnel := TunnelConfig{
			Type:       req.Type,
			Subdomain:  req.Subdomain,
			RemotePort: req.RemotePort,
			LocalAddr:  req.LocalAddr,
		}

		resp, err := i.client.AddTunnel(tunnel)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		if !resp.Success {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": resp.Error})
			return
		}

		// Persist to config if requested
		if req.Persist && i.configMgr != nil {
			entry := TunnelEntry{
				Type:       req.Type,
				Subdomain:  req.Subdomain,
				RemotePort: req.RemotePort,
				Local:      req.LocalAddr,
			}
			// Update with assigned values
			if req.Type == "http" && req.Subdomain == "" && resp.URL != "" {
				entry.Subdomain = extractSubdomainFromURL(resp.URL)
			}
			if req.Type == "tcp" && resp.AssignedPort != 0 {
				entry.RemotePort = resp.AssignedPort
			}
			if err := i.configMgr.AddTunnel(entry); err != nil {
				// Log but don't fail the request - tunnel was still added
				fmt.Printf("Warning: failed to persist tunnel config: %v\n", err)
			}
		}

		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/tunnels/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if i.client == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "Not connected to server"})
			return
		}

		var req struct {
			Type       string `json:"type"`
			Subdomain  string `json:"subdomain,omitempty"`
			RemotePort int    `json:"remote_port,omitempty"`
			Persist    bool   `json:"persist"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
			return
		}

		resp, err := i.client.RemoveTunnel(req.Type, req.Subdomain, req.RemotePort)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		if !resp.Success {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": resp.Error})
			return
		}

		// Remove from config if requested
		if req.Persist && i.configMgr != nil {
			if err := i.configMgr.RemoveTunnel(req.Type, req.Subdomain, req.RemotePort); err != nil {
				fmt.Printf("Warning: failed to update tunnel config: %v\n", err)
			}
		}

		json.NewEncoder(w).Encode(resp)
	})

	// Webhook tester endpoint - send a custom request to a tunnel
	mux.HandleFunc("/api/webhook-test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		var req struct {
			URL     string            `json:"url"`
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
			return
		}

		if req.URL == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "URL is required"})
			return
		}

		if req.Method == "" {
			req.Method = "GET"
		}

		// Create the HTTP request
		var bodyReader io.Reader
		if req.Body != "" {
			bodyReader = strings.NewReader(req.Body)
		}

		httpReq, err := http.NewRequest(req.Method, req.URL, bodyReader)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		// Set headers
		for k, v := range req.Headers {
			httpReq.Header.Set(k, v)
		}

		// Send request
		client := &http.Client{Timeout: 30 * time.Second}
		start := time.Now()
		resp, err := client.Do(httpReq)
		duration := time.Since(start)

		if err != nil {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":       err.Error(),
				"duration_ms": duration.Milliseconds(),
			})
			return
		}
		defer resp.Body.Close()

		// Read response body
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // Limit to 1MB

		// Return result
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":       resp.StatusCode,
			"status_text":  resp.Status,
			"headers":      resp.Header,
			"body":         string(respBody),
			"duration_ms":  duration.Milliseconds(),
			"content_type": resp.Header.Get("Content-Type"),
		})
	})

	// HAR export endpoint
	mux.HandleFunc("/api/export/har", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "attachment; filename=tunnel-requests.har")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		i.mu.RLock()
		requests := make([]*RequestRecord, len(i.requests))
		copy(requests, i.requests)
		i.mu.RUnlock()

		// Build HAR format
		entries := make([]map[string]interface{}, 0, len(requests))
		for _, req := range requests {
			entry := map[string]interface{}{
				"startedDateTime": req.Timestamp.Format(time.RFC3339),
				"time":            req.DurationMs,
				"request": map[string]interface{}{
					"method":      req.Method,
					"url":         req.URL,
					"httpVersion": "HTTP/1.1",
					"headers":     headersToHAR(req.ReqHeader),
					"queryString": []interface{}{},
					"bodySize":    req.ReqBodySize,
					"postData": map[string]interface{}{
						"mimeType": req.ReqHeader.Get("Content-Type"),
						"text":     req.ReqBody,
					},
				},
				"response": map[string]interface{}{
					"status":      req.Status,
					"statusText":  http.StatusText(req.Status),
					"httpVersion": "HTTP/1.1",
					"headers":     headersToHAR(req.ResHeader),
					"content": map[string]interface{}{
						"size":     req.ResBodySize,
						"mimeType": req.ContentType,
						"text":     req.ResBody,
					},
					"bodySize": req.ResBodySize,
				},
				"timings": map[string]interface{}{
					"wait": req.DurationMs,
				},
			}
			entries = append(entries, entry)
		}

		har := map[string]interface{}{
			"log": map[string]interface{}{
				"version": "1.2",
				"creator": map[string]interface{}{
					"name":    "Tunnel Inspector",
					"version": "1.0",
				},
				"entries": entries,
			},
		}

		json.NewEncoder(w).Encode(har)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(dashboardHTML))
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	fmt.Printf("\n--> Inspection Dashboard: http://%s\n", addr)

	// Start SSE broadcaster
	go i.runSSEBroadcaster(ctx)

	// Run server in goroutine
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("dashboard server error: %v\n", err)
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	server.Shutdown(shutdownCtx)
}

const dashboardHTML = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Tunnel Inspector</title>
    <style>
        :root {
            --bg: #0d1117;
            --bg-secondary: #161b22;
            --bg-tertiary: #21262d;
            --border: #30363d;
            --border-light: #3d444d;
            --text-main: #e6edf3;
            --text-secondary: #8b949e;
            --text-muted: #6e7681;
            --primary: #58a6ff;
            --primary-hover: #79b8ff;
            --success: #3fb950;
            --success-muted: #238636;
            --error: #f85149;
            --warning: #d29922;
            --http-badge: #58a6ff;
            --tcp-badge: #3fb950;
            --code-bg: #161b22;
            --tree-line: #30363d;
        }

        * { box-sizing: border-box; margin: 0; padding: 0; }

        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
            background: var(--bg);
            color: var(--text-main);
            height: 100vh;
            display: flex;
            overflow: hidden;
            font-size: 13px;
        }

        /* Monospace for code-like elements */
        .mono { font-family: 'SF Mono', 'Menlo', 'Monaco', 'Consolas', monospace; }

        /* Layout */
        .sidebar {
            width: 420px;
            background: var(--bg-secondary);
            border-right: 1px solid var(--border);
            display: flex;
            flex-direction: column;
        }
        .main {
            flex: 1;
            background: var(--bg);
            display: flex;
            flex-direction: column;
            overflow: hidden;
        }

        /* ===== TUNNEL SECTION ===== */
        .tunnel-panel {
            border-bottom: 1px solid var(--border);
            display: flex;
            flex-direction: column;
        }

        .tunnel-panel-header {
            padding: 12px 16px;
            display: flex;
            align-items: center;
            justify-content: space-between;
            background: var(--bg-tertiary);
            border-bottom: 1px solid var(--border);
            cursor: pointer;
            user-select: none;
        }

        .tunnel-panel-header:hover {
            background: #282e36;
        }

        .tunnel-panel-title {
            display: flex;
            align-items: center;
            gap: 10px;
            font-weight: 600;
            font-size: 12px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            color: var(--text-secondary);
        }

        .tunnel-panel-title .chevron {
            transition: transform 0.15s ease;
            color: var(--text-muted);
        }

        .tunnel-panel.collapsed .chevron {
            transform: rotate(-90deg);
        }

        .tunnel-count {
            background: var(--primary);
            color: var(--bg);
            font-size: 10px;
            font-weight: 700;
            padding: 2px 6px;
            border-radius: 10px;
            min-width: 18px;
            text-align: center;
        }

        .tunnel-status-dot {
            width: 8px;
            height: 8px;
            border-radius: 50%;
            background: var(--success);
            box-shadow: 0 0 6px var(--success);
            animation: pulse 2s infinite;
        }

        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.5; }
        }

        .tunnel-status-dot.disconnected {
            background: var(--error);
            box-shadow: 0 0 6px var(--error);
            animation: none;
        }

        .tunnel-panel-actions {
            display: flex;
            gap: 8px;
            align-items: center;
        }

        .btn-new-tunnel {
            background: var(--success-muted);
            color: var(--text-main);
            border: none;
            padding: 5px 10px;
            border-radius: 4px;
            cursor: pointer;
            font-size: 11px;
            font-weight: 500;
            display: flex;
            align-items: center;
            gap: 4px;
            transition: background 0.15s;
        }

        .btn-new-tunnel:hover {
            background: var(--success);
        }

        .kbd {
            font-family: 'SF Mono', monospace;
            font-size: 10px;
            background: var(--bg);
            border: 1px solid var(--border);
            border-radius: 3px;
            padding: 1px 5px;
            color: var(--text-muted);
        }

        .tunnel-panel-body {
            max-height: 350px;
            overflow-y: auto;
            transition: max-height 0.2s ease;
        }

        .tunnel-panel.collapsed .tunnel-panel-body {
            max-height: 0;
            overflow: hidden;
        }

        /* Filter */
        .tunnel-filter {
            padding: 8px 12px;
            background: var(--bg-secondary);
            border-bottom: 1px solid var(--border);
            display: none;
        }

        .tunnel-filter.visible {
            display: block;
        }

        .tunnel-filter-input {
            width: 100%;
            background: var(--bg);
            border: 1px solid var(--border);
            border-radius: 4px;
            padding: 6px 10px;
            color: var(--text-main);
            font-size: 12px;
            font-family: 'SF Mono', monospace;
        }

        .tunnel-filter-input:focus {
            outline: none;
            border-color: var(--primary);
        }

        .tunnel-filter-input::placeholder {
            color: var(--text-muted);
        }

        /* Tunnel Groups */
        .tunnel-group {
            border-bottom: 1px solid var(--border);
        }

        .tunnel-group:last-child {
            border-bottom: none;
        }

        .tunnel-group-header {
            padding: 8px 16px;
            display: flex;
            align-items: center;
            gap: 8px;
            cursor: pointer;
            background: var(--bg-secondary);
            user-select: none;
            font-size: 11px;
            font-weight: 600;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            color: var(--text-muted);
        }

        .tunnel-group-header:hover {
            background: var(--bg-tertiary);
        }

        .tunnel-group-header .chevron {
            font-size: 10px;
            transition: transform 0.15s ease;
        }

        .tunnel-group.collapsed .tunnel-group-header .chevron {
            transform: rotate(-90deg);
        }

        .tunnel-group-header .line {
            flex: 1;
            height: 1px;
            background: var(--border);
        }

        .tunnel-group-header .count {
            color: var(--text-secondary);
            font-weight: 400;
        }

        .tunnel-group-body {
            padding: 4px 0 8px 0;
        }

        .tunnel-group.collapsed .tunnel-group-body {
            display: none;
        }

        /* Tunnel Items with Tree */
        .tunnel-tree {
            padding-left: 16px;
        }

        .tunnel-item {
            display: flex;
            align-items: flex-start;
            padding: 6px 16px 6px 0;
            position: relative;
        }

        .tunnel-item:hover {
            background: var(--bg-tertiary);
        }

        .tree-connector {
            width: 24px;
            display: flex;
            flex-direction: column;
            align-items: center;
            color: var(--tree-line);
            font-family: monospace;
            flex-shrink: 0;
            padding-top: 2px;
        }

        .tree-connector .line {
            color: var(--tree-line);
            font-size: 14px;
            line-height: 1;
        }

        .tunnel-content {
            flex: 1;
            min-width: 0;
        }

        .tunnel-main-row {
            display: flex;
            align-items: center;
            gap: 8px;
        }

        .tunnel-status {
            width: 6px;
            height: 6px;
            border-radius: 50%;
            background: var(--success);
            flex-shrink: 0;
        }

        .tunnel-status.connecting {
            background: var(--warning);
            animation: pulse 1s infinite;
        }

        .tunnel-status.error {
            background: var(--error);
        }

        .tunnel-url {
            font-family: 'SF Mono', monospace;
            font-size: 12px;
            color: var(--text-main);
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
            flex: 1;
        }

        .tunnel-actions {
            display: flex;
            gap: 2px;
            opacity: 0;
            transition: opacity 0.1s;
        }

        .tunnel-item:hover .tunnel-actions {
            opacity: 1;
        }

        .tunnel-action-btn {
            background: none;
            border: none;
            color: var(--text-muted);
            cursor: pointer;
            padding: 4px 6px;
            border-radius: 3px;
            font-size: 12px;
            display: flex;
            align-items: center;
            justify-content: center;
            transition: all 0.1s;
        }

        .tunnel-action-btn:hover {
            background: var(--bg-tertiary);
            color: var(--text-main);
        }

        .tunnel-action-btn.copy-success {
            color: var(--success);
        }

        .tunnel-action-btn.remove:hover {
            color: var(--error);
        }

        /* ===== QR CODE MODAL ===== */
        .qr-modal {
            position: fixed;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: rgba(0, 0, 0, 0.7);
            display: none;
            align-items: center;
            justify-content: center;
            z-index: 1000;
        }

        .qr-modal.visible {
            display: flex;
        }

        .qr-modal-content {
            background: var(--bg-main);
            border-radius: 8px;
            padding: 24px;
            text-align: center;
            border: 1px solid var(--border);
            min-width: 280px;
        }

        .qr-modal-title {
            font-size: 14px;
            font-weight: 600;
            margin-bottom: 16px;
        }

        .qr-container {
            background: white;
            padding: 16px;
            border-radius: 8px;
            display: inline-block;
            margin-bottom: 16px;
        }

        .qr-container canvas {
            display: block;
        }

        .qr-url {
            font-size: 12px;
            color: var(--text-secondary);
            word-break: break-all;
            max-width: 250px;
            margin: 0 auto 16px;
        }

        .qr-modal-close {
            padding: 8px 16px;
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-main);
            cursor: pointer;
            font-size: 12px;
        }

        .qr-modal-close:hover {
            background: var(--border);
        }

        /* ===== WEBHOOK TESTER ===== */
        .webhook-modal {
            position: fixed;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: rgba(0, 0, 0, 0.7);
            display: none;
            align-items: center;
            justify-content: center;
            z-index: 1000;
        }

        .webhook-modal.visible {
            display: flex;
        }

        .webhook-modal-content {
            background: var(--bg-main);
            border-radius: 8px;
            width: 600px;
            max-height: 90vh;
            display: flex;
            flex-direction: column;
            border: 1px solid var(--border);
        }

        .webhook-modal-header {
            padding: 16px 20px;
            border-bottom: 1px solid var(--border);
            display: flex;
            align-items: center;
            justify-content: space-between;
        }

        .webhook-modal-title {
            font-size: 14px;
            font-weight: 600;
        }

        .webhook-modal-close {
            background: none;
            border: none;
            color: var(--text-muted);
            cursor: pointer;
            font-size: 20px;
        }

        .webhook-form {
            padding: 20px;
            overflow-y: auto;
        }

        .webhook-row {
            display: flex;
            gap: 8px;
            margin-bottom: 12px;
        }

        .webhook-method {
            width: 100px;
            padding: 8px;
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-main);
            font-size: 12px;
        }

        .webhook-url {
            flex: 1;
            padding: 8px;
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-main);
            font-size: 12px;
        }

        .webhook-label {
            font-size: 11px;
            color: var(--text-muted);
            margin-bottom: 6px;
            text-transform: uppercase;
        }

        .webhook-headers {
            width: 100%;
            height: 80px;
            padding: 8px;
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-main);
            font-family: monospace;
            font-size: 12px;
            resize: vertical;
            margin-bottom: 12px;
        }

        .webhook-body {
            width: 100%;
            height: 120px;
            padding: 8px;
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-main);
            font-family: monospace;
            font-size: 12px;
            resize: vertical;
            margin-bottom: 16px;
        }

        .webhook-actions {
            display: flex;
            gap: 8px;
            justify-content: flex-end;
        }

        .btn-webhook-send {
            padding: 8px 16px;
            background: var(--primary);
            color: white;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 12px;
            font-weight: 500;
        }

        .btn-webhook-send:hover {
            background: #4a9eff;
        }

        .btn-webhook-send:disabled {
            opacity: 0.5;
            cursor: not-allowed;
        }

        .webhook-response {
            margin-top: 16px;
            padding-top: 16px;
            border-top: 1px solid var(--border);
            display: none;
        }

        .webhook-response.visible {
            display: block;
        }

        .webhook-response-header {
            display: flex;
            align-items: center;
            gap: 12px;
            margin-bottom: 8px;
        }

        .webhook-response-status {
            font-weight: 600;
            font-size: 13px;
        }

        .webhook-response-status.success {
            color: var(--success);
        }

        .webhook-response-status.error {
            color: var(--error);
        }

        .webhook-response-time {
            font-size: 11px;
            color: var(--text-muted);
        }

        .webhook-response-body {
            background: var(--bg-tertiary);
            border-radius: 4px;
            padding: 12px;
            font-family: monospace;
            font-size: 11px;
            max-height: 200px;
            overflow: auto;
            white-space: pre-wrap;
            word-break: break-all;
        }

        .tunnel-local-row {
            display: flex;
            align-items: center;
            padding-left: 14px;
            margin-top: 2px;
        }

        .tree-elbow {
            color: var(--tree-line);
            font-family: monospace;
            font-size: 12px;
            margin-right: 6px;
        }

        .tunnel-arrow {
            color: var(--text-muted);
            margin-right: 6px;
            font-size: 11px;
        }

        .tunnel-local {
            font-family: 'SF Mono', monospace;
            font-size: 11px;
            color: var(--text-secondary);
        }

        /* Empty State */
        .tunnel-empty {
            padding: 32px 16px;
            text-align: center;
            color: var(--text-muted);
        }

        .tunnel-empty-icon {
            font-size: 32px;
            margin-bottom: 12px;
            opacity: 0.5;
        }

        .tunnel-empty-title {
            font-size: 14px;
            font-weight: 500;
            color: var(--text-secondary);
            margin-bottom: 8px;
        }

        .tunnel-empty-hint {
            font-size: 12px;
            color: var(--text-muted);
            margin-bottom: 16px;
        }

        .btn-empty-add {
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            color: var(--text-main);
            padding: 8px 16px;
            border-radius: 6px;
            cursor: pointer;
            font-size: 12px;
            transition: all 0.15s;
        }

        .btn-empty-add:hover {
            background: var(--primary);
            border-color: var(--primary);
        }

        /* Footer Status Bar */
        .tunnel-footer {
            padding: 8px 16px;
            background: var(--bg);
            border-top: 1px solid var(--border);
            display: flex;
            align-items: center;
            justify-content: space-between;
            font-size: 11px;
            color: var(--text-muted);
        }

        .tunnel-footer-left {
            display: flex;
            align-items: center;
            gap: 8px;
        }

        .tunnel-footer-status {
            display: flex;
            align-items: center;
            gap: 6px;
        }

        .tunnel-footer-right {
            display: flex;
            align-items: center;
            gap: 12px;
        }

        .tunnel-footer-config {
            font-family: 'SF Mono', monospace;
            color: var(--text-secondary);
            max-width: 200px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }

        .btn-reload {
            background: none;
            border: none;
            color: var(--text-muted);
            cursor: pointer;
            padding: 2px;
            font-size: 14px;
            transition: color 0.15s;
        }

        .btn-reload:hover {
            color: var(--text-main);
        }

        /* ===== METRICS PANEL ===== */
        .metrics-panel {
            padding: 12px 16px;
            border-bottom: 1px solid var(--border);
            background: var(--bg-secondary);
        }

        .metrics-panel-header {
            display: flex;
            align-items: center;
            justify-content: space-between;
            margin-bottom: 10px;
            cursor: pointer;
        }

        .metrics-panel-title {
            font-size: 11px;
            font-weight: 600;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            color: var(--text-secondary);
        }

        .metrics-charts {
            display: flex;
            gap: 8px;
            margin-bottom: 8px;
        }

        .metrics-chart-container {
            flex: 1;
        }

        .metrics-chart-label {
            font-size: 9px;
            color: var(--text-muted);
            margin-bottom: 4px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
        }

        .metrics-chart {
            height: 40px;
            background: var(--bg-tertiary);
            border-radius: 4px;
            overflow: hidden;
        }

        .metrics-chart svg {
            width: 100%;
            height: 100%;
        }

        .rate-bar {
            fill: var(--primary);
            opacity: 0.7;
        }

        .rate-bar:last-child {
            opacity: 1;
        }

        .metrics-stats {
            display: flex;
            justify-content: space-between;
            font-size: 11px;
            color: var(--text-secondary);
        }

        .metrics-stat {
            display: flex;
            align-items: center;
            gap: 4px;
        }

        .metrics-stat .down { color: var(--success); }
        .metrics-stat .up { color: var(--primary); }
        .metrics-stat .err { color: var(--error); }

        /* ===== FILTER BAR ===== */
        .filter-bar {
            padding: 8px 16px;
            border-bottom: 1px solid var(--border);
            display: flex;
            gap: 8px;
            background: var(--bg-secondary);
        }

        .filter-input {
            flex: 1;
            padding: 6px 10px;
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-main);
            font-size: 12px;
        }

        .filter-input:focus {
            outline: none;
            border-color: var(--primary);
        }

        .filter-input::placeholder {
            color: var(--text-muted);
        }

        .filter-select {
            padding: 6px 8px;
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-main);
            font-size: 12px;
            cursor: pointer;
        }

        .filter-select:focus {
            outline: none;
            border-color: var(--primary);
        }

        /* ===== SYNTAX HIGHLIGHTING ===== */
        .json-key { color: #79b8ff; }
        .json-string { color: #a5d6ff; }
        .json-number { color: #f0883e; }
        .json-bool { color: #ff7b72; }
        .json-null { color: #ff7b72; }
        .xml-tag { color: #7ee787; }
        .xml-attr { color: #79b8ff; }

        /* ===== TRAFFIC SECTION ===== */
        .traffic-header {
            padding: 12px 16px;
            display: flex;
            align-items: center;
            justify-content: space-between;
            border-bottom: 1px solid var(--border);
            background: var(--bg-tertiary);
        }

        .traffic-header h1 {
            font-size: 12px;
            font-weight: 600;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            color: var(--text-secondary);
        }

        .traffic-header-actions {
            display: flex;
            align-items: center;
            gap: 8px;
        }

        .btn-export-har {
            background: none;
            border: none;
            cursor: pointer;
            padding: 4px;
            border-radius: 4px;
            opacity: 0.5;
            transition: opacity 0.1s;
        }

        .btn-export-har:hover {
            opacity: 0.8;
        }

        .btn-notify {
            background: none;
            border: none;
            cursor: pointer;
            padding: 4px;
            border-radius: 4px;
            opacity: 0.5;
            transition: opacity 0.1s;
        }

        .btn-notify:hover {
            opacity: 0.8;
        }

        .btn-notify.active {
            opacity: 1;
        }

        .btn-notify.active .notify-icon {
            animation: bell-ring 0.5s ease;
        }

        @keyframes bell-ring {
            0%, 100% { transform: rotate(0); }
            25% { transform: rotate(15deg); }
            75% { transform: rotate(-15deg); }
        }

        .traffic-live-badge {
            display: flex;
            align-items: center;
            gap: 6px;
            font-size: 11px;
            padding: 3px 8px;
            border-radius: 12px;
            background: rgba(63, 185, 80, 0.15);
            color: var(--success);
        }

        .traffic-live-dot {
            width: 6px;
            height: 6px;
            border-radius: 50%;
            background: var(--success);
            animation: pulse 2s infinite;
        }

        .traffic-live-badge.reconnecting {
            background: rgba(210, 153, 34, 0.15);
            color: var(--warning);
        }

        .traffic-live-badge.reconnecting .traffic-live-dot {
            background: var(--warning);
        }

        /* Request List */
        .req-list {
            flex: 1;
            overflow-y: auto;
        }

        .req-item {
            padding: 10px 16px;
            border-bottom: 1px solid var(--border);
            cursor: pointer;
            display: flex;
            align-items: center;
            gap: 12px;
            transition: background 0.1s;
        }

        .req-item:hover {
            background: var(--bg-tertiary);
        }

        .req-item.active {
            background: rgba(88, 166, 255, 0.1);
            border-left: 2px solid var(--primary);
        }

        .req-status-dot {
            width: 8px;
            height: 8px;
            border-radius: 50%;
            flex-shrink: 0;
        }

        .req-status-dot.s-2xx { background: var(--success); }
        .req-status-dot.s-4xx { background: var(--warning); }
        .req-status-dot.s-5xx { background: var(--error); }

        .req-info {
            flex: 1;
            min-width: 0;
        }

        .req-main {
            display: flex;
            align-items: center;
            gap: 8px;
            margin-bottom: 3px;
        }

        .req-method {
            font-family: 'SF Mono', monospace;
            font-size: 10px;
            font-weight: 700;
            padding: 2px 4px;
            border-radius: 3px;
            background: var(--bg-tertiary);
            color: var(--text-secondary);
        }

        .req-method.s-2xx { color: var(--success); }
        .req-method.s-4xx { color: var(--warning); }
        .req-method.s-5xx { color: var(--error); }

        .req-path {
            font-family: 'SF Mono', monospace;
            font-size: 12px;
            color: var(--text-main);
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        .req-meta {
            display: flex;
            gap: 12px;
            font-size: 11px;
            color: var(--text-muted);
        }

        /* ===== DETAIL VIEW ===== */
        .empty-state {
            flex: 1;
            display: flex;
            flex-direction: column;
            align-items: center;
            justify-content: center;
            color: var(--text-muted);
            gap: 8px;
        }

        .empty-state-icon {
            font-size: 48px;
            opacity: 0.3;
        }

        .detail-view {
            display: none;
            flex-direction: column;
            height: 100%;
        }

        .detail-view.visible {
            display: flex;
        }

        .detail-header {
            padding: 16px 24px;
            border-bottom: 1px solid var(--border);
            background: var(--bg-secondary);
        }

        .detail-title {
            display: flex;
            align-items: center;
            gap: 12px;
            margin-bottom: 8px;
        }

        .detail-method {
            font-family: 'SF Mono', monospace;
            font-size: 12px;
            font-weight: 700;
            padding: 3px 8px;
            border-radius: 4px;
            background: var(--bg-tertiary);
        }

        .detail-path {
            font-family: 'SF Mono', monospace;
            font-size: 14px;
            color: var(--text-main);
        }

        .detail-meta {
            display: flex;
            align-items: center;
            gap: 16px;
            font-size: 12px;
            color: var(--text-secondary);
        }

        .detail-status {
            font-weight: 600;
        }

        .detail-status.s-2xx { color: var(--success); }
        .detail-status.s-4xx { color: var(--warning); }
        .detail-status.s-5xx { color: var(--error); }

        .btn-replay {
            background: var(--primary);
            color: var(--bg);
            border: none;
            padding: 5px 12px;
            border-radius: 4px;
            cursor: pointer;
            font-size: 11px;
            font-weight: 600;
            margin-left: auto;
            transition: background 0.15s;
        }

        .btn-replay:hover {
            background: var(--primary-hover);
        }

        .btn-replay:disabled {
            background: var(--bg-tertiary);
            color: var(--text-muted);
            cursor: not-allowed;
        }

        .export-dropdown {
            position: relative;
            display: inline-block;
        }

        .btn-export {
            background: var(--bg-tertiary);
            color: var(--text-main);
            border: 1px solid var(--border);
            padding: 5px 12px;
            border-radius: 4px;
            cursor: pointer;
            font-size: 11px;
            font-weight: 500;
            transition: all 0.15s;
        }

        .btn-export:hover {
            background: var(--border);
        }

        .export-menu {
            display: none;
            position: absolute;
            top: 100%;
            right: 0;
            margin-top: 4px;
            background: var(--bg-secondary);
            border: 1px solid var(--border);
            border-radius: 6px;
            min-width: 140px;
            box-shadow: 0 8px 24px rgba(0,0,0,0.4);
            z-index: 100;
        }

        .export-menu.visible {
            display: block;
        }

        .export-option {
            padding: 8px 12px;
            cursor: pointer;
            font-size: 12px;
            transition: background 0.1s;
        }

        .export-option:hover {
            background: var(--bg-tertiary);
        }

        .export-option:first-child {
            border-radius: 6px 6px 0 0;
        }

        .export-option:last-child {
            border-radius: 0 0 6px 6px;
        }

        .copy-toast {
            position: fixed;
            bottom: 20px;
            left: 50%;
            transform: translateX(-50%);
            background: var(--success);
            color: white;
            padding: 8px 16px;
            border-radius: 4px;
            font-size: 12px;
            z-index: 1000;
            opacity: 0;
            transition: opacity 0.2s;
        }

        .copy-toast.visible {
            opacity: 1;
        }

        .replay-result {
            margin-top: 12px;
            padding: 10px 12px;
            border-radius: 6px;
            font-size: 12px;
            display: none;
        }

        .replay-result.visible {
            display: block;
        }

        .replay-result.success {
            background: rgba(63, 185, 80, 0.1);
            border: 1px solid rgba(63, 185, 80, 0.3);
            color: var(--success);
        }

        .replay-result.error {
            background: rgba(248, 81, 73, 0.1);
            border: 1px solid rgba(248, 81, 73, 0.3);
            color: var(--error);
        }

        /* ===== DIFF COMPARISON ===== */
        .diff-checkbox {
            width: 14px;
            height: 14px;
            cursor: pointer;
            accent-color: var(--primary);
            flex-shrink: 0;
        }

        .diff-checkbox:checked + .req-status-dot {
            margin-left: 0;
        }

        .compare-bar {
            padding: 8px 16px;
            background: rgba(88, 166, 255, 0.1);
            border-bottom: 1px solid var(--primary);
            display: none;
            align-items: center;
            justify-content: space-between;
            font-size: 12px;
        }

        .compare-bar.visible {
            display: flex;
        }

        .compare-bar-info {
            color: var(--text-secondary);
        }

        .compare-bar-actions {
            display: flex;
            gap: 8px;
        }

        .btn-compare {
            padding: 4px 12px;
            background: var(--primary);
            color: white;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 11px;
            font-weight: 500;
        }

        .btn-compare:hover {
            background: #4a9eff;
        }

        .btn-clear-compare {
            padding: 4px 12px;
            background: transparent;
            color: var(--text-muted);
            border: 1px solid var(--border);
            border-radius: 4px;
            cursor: pointer;
            font-size: 11px;
        }

        .btn-clear-compare:hover {
            background: var(--bg-tertiary);
        }

        .diff-modal {
            position: fixed;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: rgba(0, 0, 0, 0.7);
            display: none;
            align-items: center;
            justify-content: center;
            z-index: 1000;
        }

        .diff-modal.visible {
            display: flex;
        }

        .diff-modal-content {
            background: var(--bg-main);
            border-radius: 8px;
            width: 95%;
            max-width: 1400px;
            max-height: 90vh;
            display: flex;
            flex-direction: column;
            border: 1px solid var(--border);
        }

        .diff-modal-header {
            padding: 16px 20px;
            border-bottom: 1px solid var(--border);
            display: flex;
            align-items: center;
            justify-content: space-between;
        }

        .diff-modal-title {
            font-size: 14px;
            font-weight: 600;
        }

        .diff-modal-close {
            background: none;
            border: none;
            color: var(--text-muted);
            cursor: pointer;
            font-size: 20px;
            padding: 4px 8px;
        }

        .diff-modal-close:hover {
            color: var(--text-main);
        }

        .diff-tabs {
            display: flex;
            gap: 0;
            padding: 0 20px;
            border-bottom: 1px solid var(--border);
        }

        .diff-tab {
            padding: 10px 16px;
            cursor: pointer;
            font-size: 12px;
            color: var(--text-muted);
            border-bottom: 2px solid transparent;
            margin-bottom: -1px;
        }

        .diff-tab:hover {
            color: var(--text-main);
        }

        .diff-tab.active {
            color: var(--primary);
            border-bottom-color: var(--primary);
        }

        .diff-body {
            flex: 1;
            overflow: hidden;
            display: flex;
        }

        .diff-pane {
            flex: 1;
            overflow: auto;
            padding: 16px;
        }

        .diff-pane:first-child {
            border-right: 1px solid var(--border);
        }

        .diff-pane-header {
            font-size: 11px;
            font-weight: 600;
            text-transform: uppercase;
            color: var(--text-muted);
            margin-bottom: 12px;
            padding-bottom: 8px;
            border-bottom: 1px solid var(--border);
        }

        .diff-line {
            font-family: 'SF Mono', Monaco, 'Cascadia Code', monospace;
            font-size: 12px;
            line-height: 1.6;
            padding: 2px 8px;
            white-space: pre-wrap;
            word-break: break-all;
        }

        .diff-line.added {
            background: rgba(63, 185, 80, 0.15);
            color: var(--success);
        }

        .diff-line.removed {
            background: rgba(248, 81, 73, 0.15);
            color: var(--error);
        }

        .diff-line.unchanged {
            color: var(--text-secondary);
        }

        .detail-content {
            flex: 1;
            overflow-y: auto;
            padding: 20px 24px;
        }

        .detail-section {
            margin-bottom: 24px;
        }

        .section-title {
            font-size: 11px;
            font-weight: 600;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            color: var(--text-muted);
            margin-bottom: 12px;
            padding-bottom: 8px;
            border-bottom: 1px solid var(--border);
        }

        .tabs {
            display: flex;
            gap: 0;
            margin-bottom: 12px;
            border-bottom: 1px solid var(--border);
        }

        .tab {
            padding: 8px 16px;
            cursor: pointer;
            font-size: 12px;
            color: var(--text-secondary);
            border-bottom: 2px solid transparent;
            margin-bottom: -1px;
            transition: all 0.15s;
        }

        .tab:hover {
            color: var(--text-main);
        }

        .tab.active {
            color: var(--primary);
            border-bottom-color: var(--primary);
        }

        .tab-content {
            display: none;
        }

        .tab-content.active {
            display: block;
        }

        .kv-grid {
            display: grid;
            grid-template-columns: auto 1fr;
            gap: 6px 16px;
            font-size: 12px;
        }

        .kv-key {
            font-family: 'SF Mono', monospace;
            color: var(--text-muted);
            text-align: right;
        }

        .kv-val {
            font-family: 'SF Mono', monospace;
            color: var(--text-main);
            word-break: break-all;
        }

        pre {
            margin: 0;
            padding: 16px;
            background: var(--bg-secondary);
            border: 1px solid var(--border);
            border-radius: 6px;
            font-family: 'SF Mono', monospace;
            font-size: 11px;
            color: var(--text-main);
            overflow-x: auto;
            white-space: pre-wrap;
            word-break: break-all;
        }

        /* ===== MODAL ===== */
        .modal-overlay {
            position: fixed;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: rgba(0, 0, 0, 0.7);
            display: none;
            align-items: center;
            justify-content: center;
            z-index: 1000;
        }

        .modal-overlay.visible {
            display: flex;
        }

        .modal {
            background: var(--bg-secondary);
            border: 1px solid var(--border);
            border-radius: 12px;
            width: 400px;
            max-width: 90vw;
            box-shadow: 0 24px 48px rgba(0, 0, 0, 0.4);
        }

        .modal-header {
            padding: 16px 20px;
            border-bottom: 1px solid var(--border);
            display: flex;
            align-items: center;
            justify-content: space-between;
        }

        .modal-header h2 {
            font-size: 14px;
            font-weight: 600;
            color: var(--text-main);
        }

        .modal-close {
            background: none;
            border: none;
            color: var(--text-muted);
            cursor: pointer;
            font-size: 18px;
            padding: 4px;
            line-height: 1;
            transition: color 0.15s;
        }

        .modal-close:hover {
            color: var(--text-main);
        }

        .modal-body {
            padding: 20px;
        }

        .form-group {
            margin-bottom: 16px;
        }

        .form-label {
            display: block;
            font-size: 12px;
            font-weight: 500;
            color: var(--text-secondary);
            margin-bottom: 6px;
        }

        .form-input,
        .form-select {
            width: 100%;
            padding: 8px 12px;
            background: var(--bg);
            border: 1px solid var(--border);
            border-radius: 6px;
            color: var(--text-main);
            font-size: 13px;
            font-family: 'SF Mono', monospace;
            transition: border-color 0.15s;
        }

        .form-input:focus,
        .form-select:focus {
            outline: none;
            border-color: var(--primary);
        }

        .form-input::placeholder {
            color: var(--text-muted);
        }

        .form-hint {
            font-size: 11px;
            color: var(--text-muted);
            margin-top: 4px;
        }

        .form-preview {
            font-size: 11px;
            color: var(--primary);
            margin-top: 4px;
            font-family: 'SF Mono', monospace;
        }

        .form-type-selector {
            display: flex;
            gap: 8px;
        }

        .form-type-option {
            flex: 1;
            padding: 10px;
            background: var(--bg);
            border: 1px solid var(--border);
            border-radius: 6px;
            cursor: pointer;
            text-align: center;
            transition: all 0.15s;
        }

        .form-type-option:hover {
            border-color: var(--primary);
        }

        .form-type-option.selected {
            border-color: var(--primary);
            background: rgba(88, 166, 255, 0.1);
        }

        .form-type-option input {
            display: none;
        }

        .form-type-option .type-label {
            font-size: 12px;
            font-weight: 600;
            color: var(--text-main);
        }

        .form-type-option .type-desc {
            font-size: 10px;
            color: var(--text-muted);
            margin-top: 2px;
        }

        .form-checkbox {
            display: flex;
            align-items: center;
            gap: 8px;
            cursor: pointer;
        }

        .form-checkbox input {
            width: 14px;
            height: 14px;
            accent-color: var(--primary);
        }

        .form-checkbox span {
            font-size: 12px;
            color: var(--text-secondary);
        }

        .modal-footer {
            padding: 16px 20px;
            border-top: 1px solid var(--border);
            display: flex;
            justify-content: flex-end;
            gap: 8px;
        }

        .btn-modal-cancel {
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            color: var(--text-secondary);
            padding: 8px 16px;
            border-radius: 6px;
            cursor: pointer;
            font-size: 12px;
            transition: all 0.15s;
        }

        .btn-modal-cancel:hover {
            background: var(--bg);
            color: var(--text-main);
        }

        .btn-modal-submit {
            background: var(--primary);
            border: none;
            color: var(--bg);
            padding: 8px 16px;
            border-radius: 6px;
            cursor: pointer;
            font-size: 12px;
            font-weight: 600;
            transition: background 0.15s;
        }

        .btn-modal-submit:hover {
            background: var(--primary-hover);
        }

        .btn-modal-submit:disabled {
            background: var(--bg-tertiary);
            color: var(--text-muted);
            cursor: not-allowed;
        }

        /* Scrollbar */
        ::-webkit-scrollbar {
            width: 8px;
            height: 8px;
        }

        ::-webkit-scrollbar-track {
            background: transparent;
        }

        ::-webkit-scrollbar-thumb {
            background: var(--border);
            border-radius: 4px;
        }

        ::-webkit-scrollbar-thumb:hover {
            background: var(--border-light);
        }
    </style>
</head>
<body>
    <div class="sidebar">
        <!-- Tunnel Panel -->
        <div class="tunnel-panel" id="tunnel-panel">
            <div class="tunnel-panel-header" onclick="toggleTunnelPanel()">
                <div class="tunnel-panel-title">
                    <span class="chevron">▼</span>
                    <span>Tunnels</span>
                    <span class="tunnel-count" id="tunnel-count">0</span>
                </div>
                <div class="tunnel-panel-actions" onclick="event.stopPropagation()">
                    <div class="tunnel-status-dot" id="connection-status" title="Connected"></div>
                    <button class="btn-new-tunnel" onclick="showAddTunnelModal()" title="Add new tunnel">
                        <span>+</span> New
                        <span class="kbd">N</span>
                    </button>
                </div>
            </div>

            <div class="tunnel-panel-body">
                <div class="tunnel-filter" id="tunnel-filter">
                    <input type="text" class="tunnel-filter-input" id="tunnel-filter-input"
                           placeholder="Filter tunnels... (Ctrl+K)"
                           oninput="filterTunnels(this.value)">
                </div>

                <div id="tunnel-content">
                    <!-- Tunnel groups rendered here -->
                </div>
            </div>

            <div class="tunnel-footer">
                <div class="tunnel-footer-left">
                    <div class="tunnel-footer-status">
                        <span id="tunnel-status-text">● Connected</span>
                    </div>
                </div>
                <div class="tunnel-footer-right">
                    <span class="tunnel-footer-config" id="config-path" title="Config file">~/.tunnel/tunnel.yaml</span>
                    <button class="btn-reload" onclick="loadTunnels()" title="Reload tunnels">⟳</button>
                </div>
            </div>
        </div>

        <!-- Metrics Panel -->
        <div class="metrics-panel" id="metrics-panel">
            <div class="metrics-panel-header" onclick="toggleMetricsPanel()">
                <span class="metrics-panel-title">📊 Metrics</span>
                <span class="chevron" id="metrics-chevron">▼</span>
            </div>
            <div class="metrics-panel-body" id="metrics-body">
                <div class="metrics-charts">
                    <div class="metrics-chart-container">
                        <div class="metrics-chart-label">Latency (ms)</div>
                        <div class="metrics-chart" id="latency-chart">
                            <svg id="latency-svg"></svg>
                        </div>
                    </div>
                    <div class="metrics-chart-container">
                        <div class="metrics-chart-label">Rate (req/s)</div>
                        <div class="metrics-chart rate-chart" id="rate-chart">
                            <svg id="rate-svg"></svg>
                        </div>
                    </div>
                </div>
                <div class="metrics-stats">
                    <span class="metrics-stat"><span class="down">↓</span> <span id="bytes-in">0 B</span></span>
                    <span class="metrics-stat"><span class="up">↑</span> <span id="bytes-out">0 B</span></span>
                    <span class="metrics-stat"><span id="req-rate">0</span> req/min</span>
                    <span class="metrics-stat"><span class="err" id="error-count">0</span> errors</span>
                </div>
            </div>
        </div>

        <!-- Traffic Section -->
        <div class="traffic-header">
            <h1>Traffic</h1>
            <div class="traffic-header-actions">
                <button class="btn-export-har" onclick="exportHAR()" title="Export as HAR file">
                    <span>📥</span>
                </button>
                <button class="btn-notify" id="btn-notify" onclick="toggleNotifications()" title="Enable desktop notifications for errors">
                    <span class="notify-icon">🔔</span>
                </button>
                <div class="traffic-live-badge" id="traffic-badge">
                    <div class="traffic-live-dot"></div>
                    <span>Live</span>
                </div>
            </div>
        </div>

        <!-- Filter Bar -->
        <div class="filter-bar">
            <input type="text" class="filter-input" id="req-filter-input"
                   placeholder="Filter by path..." oninput="filterRequests()">
            <select class="filter-select" id="method-filter" onchange="filterRequests()">
                <option value="">All Methods</option>
                <option value="GET">GET</option>
                <option value="POST">POST</option>
                <option value="PUT">PUT</option>
                <option value="DELETE">DELETE</option>
                <option value="PATCH">PATCH</option>
            </select>
            <select class="filter-select" id="status-filter" onchange="filterRequests()">
                <option value="">All Status</option>
                <option value="2xx">2xx Success</option>
                <option value="4xx">4xx Client Error</option>
                <option value="5xx">5xx Server Error</option>
            </select>
        </div>

        <!-- Compare Bar -->
        <div id="compare-bar" class="compare-bar">
            <span class="compare-bar-info"><span id="compare-count">0</span> requests selected for comparison</span>
            <div class="compare-bar-actions">
                <button class="btn-clear-compare" onclick="clearDiffSelection()">Clear</button>
                <button class="btn-compare" id="btn-compare" onclick="showDiffModal()">Compare</button>
            </div>
        </div>

        <div id="req-list" class="req-list"></div>
    </div>

    <!-- Add Tunnel Modal -->
    <div class="modal-overlay" id="add-tunnel-modal">
        <div class="modal">
            <div class="modal-header">
                <h2>New Tunnel</h2>
                <button class="modal-close" onclick="hideAddTunnelModal()">✕</button>
            </div>
            <form onsubmit="submitAddTunnel(event)">
                <div class="modal-body">
                    <div class="form-group">
                        <label class="form-label">Type</label>
                        <div class="form-type-selector">
                            <label class="form-type-option selected" id="type-http">
                                <input type="radio" name="tunnel-type" value="http" checked onchange="updateFormFields()">
                                <div class="type-label">HTTP</div>
                                <div class="type-desc">Web traffic</div>
                            </label>
                            <label class="form-type-option" id="type-tcp">
                                <input type="radio" name="tunnel-type" value="tcp" onchange="updateFormFields()">
                                <div class="type-label">TCP</div>
                                <div class="type-desc">Raw TCP/SSH</div>
                            </label>
                        </div>
                    </div>

                    <div class="form-group" id="subdomain-group">
                        <label class="form-label">Subdomain <span style="color: var(--text-muted)">(optional)</span></label>
                        <input type="text" class="form-input" id="tunnel-subdomain" placeholder="my-app" oninput="updatePreview()">
                        <div class="form-preview" id="subdomain-preview">→ random.px.csam.dev</div>
                    </div>

                    <div class="form-group" id="port-group" style="display: none;">
                        <label class="form-label">Remote Port <span style="color: var(--text-muted)">(0 = random)</span></label>
                        <input type="number" class="form-input" id="tunnel-port" placeholder="0" min="0" max="65535" value="0">
                        <div class="form-hint">Server will assign an available port if 0</div>
                    </div>

                    <div class="form-group">
                        <label class="form-label">Local Address</label>
                        <input type="text" class="form-input" id="tunnel-local" placeholder="127.0.0.1:3000" required>
                        <div class="form-hint">The local service to forward traffic to</div>
                    </div>

                    <div class="form-group">
                        <label class="form-checkbox">
                            <input type="checkbox" id="tunnel-persist" checked>
                            <span>Save to config file</span>
                        </label>
                    </div>
                </div>
                <div class="modal-footer">
                    <button type="button" class="btn-modal-cancel" onclick="hideAddTunnelModal()">
                        Cancel <span class="kbd">Esc</span>
                    </button>
                    <button type="submit" class="btn-modal-submit" id="btn-add-submit">
                        Add Tunnel <span class="kbd">⏎</span>
                    </button>
                </div>
            </form>
        </div>
    </div>

    <!-- Main Content -->
    <div class="main">
        <div id="empty-state" class="empty-state">
            <div class="empty-state-icon">📡</div>
            <div>Select a request to inspect details</div>
        </div>

        <div id="detail-view" class="detail-view">
            <div class="detail-header">
                <div class="detail-title">
                    <span id="d-method" class="detail-method">GET</span>
                    <span id="d-path" class="detail-path">/path</span>
                </div>
                <div class="detail-meta">
                    <span id="d-status" class="detail-status">200</span>
                    <span id="d-time">12:00:00</span>
                    <span id="d-duration">120ms</span>
                    <button id="btn-replay" class="btn-replay" onclick="replayRequest()">▶ Replay</button>
                    <div class="export-dropdown">
                        <button class="btn-export" onclick="toggleExportMenu()">⤓ Export</button>
                        <div class="export-menu" id="export-menu">
                            <div class="export-option" onclick="copyAsCurl()">Copy as cURL</div>
                            <div class="export-option" onclick="copyAsFetch()">Copy as fetch</div>
                        </div>
                    </div>
                </div>
                <div id="replay-result" class="replay-result"></div>
            </div>

            <div class="detail-content">
                <div class="detail-section">
                    <div class="section-title">Request</div>
                    <div class="tabs">
                        <div class="tab active" onclick="switchTab('req', 'headers')">Headers</div>
                        <div class="tab" onclick="switchTab('req', 'body')">Body</div>
                    </div>
                    <div id="req-headers" class="tab-content active">
                        <div id="req-headers-list" class="kv-grid"></div>
                    </div>
                    <div id="req-body" class="tab-content">
                        <pre id="req-body-content"></pre>
                    </div>
                </div>

                <div class="detail-section">
                    <div class="section-title">Response</div>
                    <div class="tabs">
                        <div class="tab active" onclick="switchTab('res', 'headers')">Headers</div>
                        <div class="tab" onclick="switchTab('res', 'body')">Body</div>
                    </div>
                    <div id="res-headers" class="tab-content active">
                        <div id="res-headers-list" class="kv-grid"></div>
                    </div>
                    <div id="res-body" class="tab-content">
                        <pre id="res-body-content"></pre>
                    </div>
                </div>
            </div>
        </div>
    </div>

    <!-- Diff Modal -->
    <div id="diff-modal" class="diff-modal" onclick="closeDiffModal(event)">
        <div class="diff-modal-content" onclick="event.stopPropagation()">
            <div class="diff-modal-header">
                <span class="diff-modal-title">Request Comparison</span>
                <button class="diff-modal-close" onclick="closeDiffModal()">&times;</button>
            </div>
            <div class="diff-tabs">
                <div class="diff-tab active" data-diff-tab="url" onclick="switchDiffTab('url')">URL & Method</div>
                <div class="diff-tab" data-diff-tab="headers" onclick="switchDiffTab('headers')">Headers</div>
                <div class="diff-tab" data-diff-tab="body" onclick="switchDiffTab('body')">Body</div>
            </div>
            <div class="diff-body">
                <div class="diff-pane">
                    <div class="diff-pane-header" id="diff-left-header">Request 1</div>
                    <div id="diff-left-content"></div>
                </div>
                <div class="diff-pane">
                    <div class="diff-pane-header" id="diff-right-header">Request 2</div>
                    <div id="diff-right-content"></div>
                </div>
            </div>
        </div>
    </div>

    <!-- QR Code Modal -->
    <div id="qr-modal" class="qr-modal" onclick="closeQRModal(event)">
        <div class="qr-modal-content" onclick="event.stopPropagation()">
            <div class="qr-modal-title">Scan to Open</div>
            <div class="qr-container">
                <canvas id="qr-canvas" width="200" height="200"></canvas>
            </div>
            <div class="qr-url" id="qr-url"></div>
            <button class="qr-modal-close" onclick="closeQRModal()">Close</button>
        </div>
    </div>

    <!-- Webhook Tester Modal -->
    <div id="webhook-modal" class="webhook-modal" onclick="closeWebhookModal(event)">
        <div class="webhook-modal-content" onclick="event.stopPropagation()">
            <div class="webhook-modal-header">
                <span class="webhook-modal-title">Webhook Tester</span>
                <button class="webhook-modal-close" onclick="closeWebhookModal()">&times;</button>
            </div>
            <div class="webhook-form">
                <div class="webhook-row">
                    <select id="webhook-method" class="webhook-method">
                        <option value="GET">GET</option>
                        <option value="POST" selected>POST</option>
                        <option value="PUT">PUT</option>
                        <option value="PATCH">PATCH</option>
                        <option value="DELETE">DELETE</option>
                    </select>
                    <input type="text" id="webhook-url" class="webhook-url" placeholder="https://your-tunnel.example.com/webhook">
                </div>
                <div class="webhook-label">Headers (JSON)</div>
                <textarea id="webhook-headers" class="webhook-headers" placeholder='{"Content-Type": "application/json"}'></textarea>
                <div class="webhook-label">Body</div>
                <textarea id="webhook-body" class="webhook-body" placeholder='{"event": "test", "data": {}}'></textarea>
                <div class="webhook-actions">
                    <button id="btn-webhook-send" class="btn-webhook-send" onclick="sendWebhookTest()">Send Request</button>
                </div>
                <div id="webhook-response" class="webhook-response">
                    <div class="webhook-response-header">
                        <span id="webhook-response-status" class="webhook-response-status"></span>
                        <span id="webhook-response-time" class="webhook-response-time"></span>
                    </div>
                    <pre id="webhook-response-body" class="webhook-response-body"></pre>
                </div>
            </div>
        </div>
    </div>

    <script>
        // State
        let tunnelsList = [];
        let filteredTunnels = [];
        let filterQuery = '';
        let collapsedGroups = {};
        let requestsMap = {};
        let requestsList = [];
        let filteredRequestsList = [];
        let selectedId = null;
        let sseConnected = false;
        let metricsCollapsed = false;
        let metricsData = null;
        let selectedForDiff = [];
        let currentDiffTab = 'url';
        let reconnectCount = 0;
        let lastHeartbeat = null;
        let connectionStartTime = null;
        let notificationsEnabled = false;

        // ===== METRICS =====

        function fetchMetrics() {
            fetch('/api/metrics')
                .then(r => r.json())
                .then(data => {
                    metricsData = data;
                    updateMetricsUI();
                })
                .catch(err => console.error('Failed to fetch metrics:', err));
        }

        function updateMetricsUI() {
            if (!metricsData) return;

            document.getElementById('bytes-in').textContent = formatBytes(metricsData.total_bytes_in);
            document.getElementById('bytes-out').textContent = formatBytes(metricsData.total_bytes_out);
            document.getElementById('req-rate').textContent = Math.round(metricsData.requests_per_min);
            document.getElementById('error-count').textContent = metricsData.error_count;

            renderLatencyChart(metricsData.latency_history || []);
            renderRateChart(metricsData.rate_history || []);
        }

        function renderRateChart(rates) {
            const svg = document.getElementById('rate-svg');
            if (!svg) return;

            const w = svg.clientWidth || 180;
            const h = svg.clientHeight || 40;
            const padding = 2;

            // Use last 30 seconds for the bar chart
            const data = rates.slice(-30);
            if (data.length === 0 || data.every(r => r === 0)) {
                svg.innerHTML = '<text x="50%" y="50%" text-anchor="middle" fill="#6e7681" font-size="9">No traffic</text>';
                return;
            }

            const maxRate = Math.max(...data, 1);
            const barWidth = (w - padding * 2) / data.length - 1;

            let bars = '';
            data.forEach((rate, i) => {
                const x = padding + i * (barWidth + 1);
                const barHeight = (rate / maxRate) * (h - padding * 2);
                const y = h - padding - barHeight;
                bars += '<rect class="rate-bar" x="' + x + '" y="' + y + '" width="' + barWidth + '" height="' + barHeight + '" rx="1"/>';
            });

            const currentRate = data[data.length - 1] || 0;
            svg.innerHTML = bars + '<text x="' + (w - padding) + '" y="10" text-anchor="end" fill="var(--text-muted)" font-size="9">' + currentRate + '/s</text>';
        }

        function renderLatencyChart(points) {
            const svg = document.getElementById('latency-svg');
            if (!svg || points.length < 2) {
                if (svg) svg.innerHTML = '<text x="50%" y="50%" text-anchor="middle" fill="#6e7681" font-size="10">Awaiting data...</text>';
                return;
            }

            const w = svg.clientWidth || 380;
            const h = svg.clientHeight || 50;
            const padding = 4;

            const maxLatency = Math.max(...points.map(p => p.ms), 1);
            const xScale = (w - padding * 2) / (points.length - 1);
            const yScale = (h - padding * 2) / maxLatency;

            const pathData = points.map((p, i) => {
                const x = padding + i * xScale;
                const y = h - padding - p.ms * yScale;
                return (i === 0 ? 'M' : 'L') + x.toFixed(1) + ',' + y.toFixed(1);
            }).join(' ');

            const avgLatency = points.reduce((sum, p) => sum + p.ms, 0) / points.length;

            svg.innerHTML =
                '<path d="' + pathData + '" fill="none" stroke="var(--primary)" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>' +
                '<text x="' + (w - padding) + '" y="12" text-anchor="end" fill="var(--text-muted)" font-size="9">avg ' + Math.round(avgLatency) + 'ms</text>';
        }

        function toggleMetricsPanel() {
            metricsCollapsed = !metricsCollapsed;
            const body = document.getElementById('metrics-body');
            const chevron = document.getElementById('metrics-chevron');
            if (metricsCollapsed) {
                body.style.display = 'none';
                chevron.style.transform = 'rotate(-90deg)';
            } else {
                body.style.display = 'block';
                chevron.style.transform = 'rotate(0deg)';
            }
        }

        function formatBytes(bytes) {
            if (bytes === 0) return '0 B';
            const k = 1024;
            const sizes = ['B', 'KB', 'MB', 'GB'];
            const i = Math.floor(Math.log(bytes) / Math.log(k));
            return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
        }

        // Start metrics polling
        setInterval(fetchMetrics, 2000);

        // ===== REQUEST FILTERING =====

        function filterRequests() {
            const pathFilter = document.getElementById('req-filter-input').value.toLowerCase();
            const methodFilter = document.getElementById('method-filter').value;
            const statusFilter = document.getElementById('status-filter').value;

            filteredRequestsList = requestsList.filter(req => {
                // Path filter
                if (pathFilter && !req.path.toLowerCase().includes(pathFilter)) {
                    return false;
                }
                // Method filter
                if (methodFilter && req.method !== methodFilter) {
                    return false;
                }
                // Status filter
                if (statusFilter) {
                    const status = req.status;
                    if (statusFilter === '2xx' && (status < 200 || status >= 300)) return false;
                    if (statusFilter === '4xx' && (status < 400 || status >= 500)) return false;
                    if (statusFilter === '5xx' && (status < 500 || status >= 600)) return false;
                }
                return true;
            });

            renderFilteredRequests();
        }

        function renderFilteredRequests() {
            const container = document.getElementById('req-list');
            container.innerHTML = '';
            filteredRequestsList.forEach(req => {
                container.appendChild(createRequestItem(req));
            });
        }

        // ===== SYNTAX HIGHLIGHTING =====

        function highlightJSON(str) {
            try {
                const obj = JSON.parse(str);
                str = JSON.stringify(obj, null, 2);
            } catch (e) {
                // Not valid JSON, return as-is
                return escapeHtml(str);
            }
            return escapeHtml(str)
                .replace(/"([^"]+)":/g, '<span class="json-key">"$1"</span>:')
                .replace(/: "([^"]*)"/g, ': <span class="json-string">"$1"</span>')
                .replace(/: (-?\d+\.?\d*)/g, ': <span class="json-number">$1</span>')
                .replace(/: (true|false)/g, ': <span class="json-bool">$1</span>')
                .replace(/: (null)/g, ': <span class="json-null">$1</span>');
        }

        function escapeHtml(str) {
            return str
                .replace(/&/g, '&amp;')
                .replace(/</g, '&lt;')
                .replace(/>/g, '&gt;')
                .replace(/"/g, '&quot;');
        }

        function formatBody(body, contentType) {
            if (!body) return '<span style="color: var(--text-muted)">No content</span>';
            contentType = contentType || '';
            if (contentType.includes('json') || body.trim().startsWith('{') || body.trim().startsWith('[')) {
                return highlightJSON(body);
            }
            return escapeHtml(body);
        }

        // ===== EXPORT FUNCTIONS =====

        function toggleExportMenu() {
            const menu = document.getElementById('export-menu');
            menu.classList.toggle('visible');

            // Close menu when clicking outside
            if (menu.classList.contains('visible')) {
                setTimeout(() => {
                    document.addEventListener('click', closeExportMenu);
                }, 0);
            }
        }

        function closeExportMenu(e) {
            const menu = document.getElementById('export-menu');
            const dropdown = e.target.closest('.export-dropdown');
            if (!dropdown) {
                menu.classList.remove('visible');
                document.removeEventListener('click', closeExportMenu);
            }
        }

        function generateCurl(r) {
            let cmd = 'curl';

            // Method
            if (r.method !== 'GET') {
                cmd += ' -X ' + r.method;
            }

            // Headers
            if (r.req_header) {
                for (const [key, values] of Object.entries(r.req_header)) {
                    if (key.toLowerCase() === 'host') continue;
                    const val = Array.isArray(values) ? values[0] : values;
                    cmd += " \\\n  -H '" + key + ': ' + val.replace(/'/g, "'\\''") + "'";
                }
            }

            // Body
            if (r.req_body) {
                cmd += " \\\n  -d '" + r.req_body.replace(/'/g, "'\\''") + "'";
            }

            // URL
            cmd += " \\\n  '" + r.url + "'";

            return cmd;
        }

        function generateFetch(r) {
            const options = {
                method: r.method
            };

            // Headers
            if (r.req_header) {
                options.headers = {};
                for (const [key, values] of Object.entries(r.req_header)) {
                    if (key.toLowerCase() === 'host') continue;
                    options.headers[key] = Array.isArray(values) ? values[0] : values;
                }
            }

            // Body
            if (r.req_body) {
                options.body = r.req_body;
            }

            return "fetch('" + r.url + "', " + JSON.stringify(options, null, 2) + ")\n  .then(res => res.json())\n  .then(console.log)\n  .catch(console.error);";
        }

        function copyAsCurl() {
            if (!selectedId) return;
            const r = requestsMap[selectedId];
            if (!r) return;

            const curl = generateCurl(r);
            copyToClipboard(curl);
            document.getElementById('export-menu').classList.remove('visible');
        }

        function copyAsFetch() {
            if (!selectedId) return;
            const r = requestsMap[selectedId];
            if (!r) return;

            const fetchCode = generateFetch(r);
            copyToClipboard(fetchCode);
            document.getElementById('export-menu').classList.remove('visible');
        }

        function copyToClipboard(text) {
            navigator.clipboard.writeText(text).then(() => {
                showCopyToast('Copied to clipboard!');
            }).catch(err => {
                console.error('Failed to copy:', err);
            });
        }

        function showCopyToast(message) {
            let toast = document.getElementById('copy-toast');
            if (!toast) {
                toast = document.createElement('div');
                toast.id = 'copy-toast';
                toast.className = 'copy-toast';
                document.body.appendChild(toast);
            }
            toast.textContent = message;
            toast.classList.add('visible');
            setTimeout(() => {
                toast.classList.remove('visible');
            }, 2000);
        }

        // ===== DIFF COMPARISON =====

        function toggleDiffSelection(id, checked) {
            if (checked) {
                if (selectedForDiff.length < 2) {
                    selectedForDiff.push(id);
                } else {
                    // Already have 2, replace the oldest
                    selectedForDiff.shift();
                    selectedForDiff.push(id);
                    // Update checkboxes in UI
                    renderRequests();
                }
            } else {
                selectedForDiff = selectedForDiff.filter(x => x !== id);
            }
            updateCompareBar();
        }

        function updateCompareBar() {
            const bar = document.getElementById('compare-bar');
            const countEl = document.getElementById('compare-count');
            const btnCompare = document.getElementById('btn-compare');

            countEl.textContent = selectedForDiff.length;

            if (selectedForDiff.length > 0) {
                bar.classList.add('visible');
            } else {
                bar.classList.remove('visible');
            }

            btnCompare.disabled = selectedForDiff.length !== 2;
            btnCompare.style.opacity = selectedForDiff.length === 2 ? '1' : '0.5';
        }

        function clearDiffSelection() {
            selectedForDiff = [];
            updateCompareBar();
            renderRequests();
        }

        function showDiffModal() {
            if (selectedForDiff.length !== 2) return;

            const modal = document.getElementById('diff-modal');
            modal.classList.add('visible');
            currentDiffTab = 'url';
            updateDiffTabs();
            renderDiffContent();
        }

        function closeDiffModal(event) {
            if (event && event.target !== event.currentTarget) return;
            document.getElementById('diff-modal').classList.remove('visible');
        }

        function switchDiffTab(tab) {
            currentDiffTab = tab;
            updateDiffTabs();
            renderDiffContent();
        }

        function updateDiffTabs() {
            document.querySelectorAll('.diff-tab').forEach(el => {
                el.classList.toggle('active', el.dataset.diffTab === currentDiffTab);
            });
        }

        function renderDiffContent() {
            const req1 = requestsMap[selectedForDiff[0]];
            const req2 = requestsMap[selectedForDiff[1]];

            if (!req1 || !req2) return;

            const leftHeader = document.getElementById('diff-left-header');
            const rightHeader = document.getElementById('diff-right-header');
            const leftContent = document.getElementById('diff-left-content');
            const rightContent = document.getElementById('diff-right-content');

            leftHeader.textContent = req1.method + ' ' + req1.path + ' (' + new Date(req1.timestamp).toLocaleTimeString() + ')';
            rightHeader.textContent = req2.method + ' ' + req2.path + ' (' + new Date(req2.timestamp).toLocaleTimeString() + ')';

            let left = '', right = '';

            switch (currentDiffTab) {
                case 'url':
                    left = formatForDiff('Method: ' + req1.method + '\nURL: ' + req1.url + '\nPath: ' + req1.path + '\nStatus: ' + req1.status + '\nDuration: ' + req1.duration);
                    right = formatForDiff('Method: ' + req2.method + '\nURL: ' + req2.url + '\nPath: ' + req2.path + '\nStatus: ' + req2.status + '\nDuration: ' + req2.duration);
                    break;
                case 'headers':
                    left = formatForDiff(formatHeaders(req1.req_header || {}));
                    right = formatForDiff(formatHeaders(req2.req_header || {}));
                    break;
                case 'body':
                    left = formatForDiff(formatBody(req1.req_body));
                    right = formatForDiff(formatBody(req2.req_body));
                    break;
            }

            const { leftHtml, rightHtml } = computeDiff(left, right);
            leftContent.innerHTML = leftHtml;
            rightContent.innerHTML = rightHtml;
        }

        function formatHeaders(headers) {
            if (!headers || Object.keys(headers).length === 0) return '(no headers)';
            return Object.entries(headers)
                .map(([k, v]) => k + ': ' + (Array.isArray(v) ? v.join(', ') : v))
                .sort()
                .join('\n');
        }

        function formatBody(body) {
            if (!body) return '(no body)';
            try {
                return JSON.stringify(JSON.parse(body), null, 2);
            } catch {
                return body;
            }
        }

        function formatForDiff(text) {
            return text.split('\n');
        }

        function computeDiff(leftLines, rightLines) {
            const maxLen = Math.max(leftLines.length, rightLines.length);
            let leftHtml = '';
            let rightHtml = '';

            for (let i = 0; i < maxLen; i++) {
                const l = leftLines[i] || '';
                const r = rightLines[i] || '';

                if (l === r) {
                    leftHtml += '<div class="diff-line unchanged">' + escapeHtml(l) + '</div>';
                    rightHtml += '<div class="diff-line unchanged">' + escapeHtml(r) + '</div>';
                } else if (!l && r) {
                    leftHtml += '<div class="diff-line removed">&nbsp;</div>';
                    rightHtml += '<div class="diff-line added">' + escapeHtml(r) + '</div>';
                } else if (l && !r) {
                    leftHtml += '<div class="diff-line removed">' + escapeHtml(l) + '</div>';
                    rightHtml += '<div class="diff-line added">&nbsp;</div>';
                } else {
                    leftHtml += '<div class="diff-line removed">' + escapeHtml(l) + '</div>';
                    rightHtml += '<div class="diff-line added">' + escapeHtml(r) + '</div>';
                }
            }

            return { leftHtml, rightHtml };
        }

        function escapeHtml(text) {
            const div = document.createElement('div');
            div.textContent = text;
            return div.innerHTML;
        }

        // ===== QR CODE =====

        function showQRCode(url) {
            document.getElementById('qr-url').textContent = url;
            document.getElementById('qr-modal').classList.add('visible');
            generateQRCode(url);
        }

        function closeQRModal(event) {
            if (event && event.target !== event.currentTarget) return;
            document.getElementById('qr-modal').classList.remove('visible');
        }

        // Render QR code using Google Charts API (simple fallback)
        function generateQRCode(text) {
            const canvas = document.getElementById('qr-canvas');
            const ctx = canvas.getContext('2d');
            const size = 200;

            // Use an image from Google Charts QR API
            const img = new Image();
            img.crossOrigin = 'anonymous';
            img.onload = function() {
                ctx.fillStyle = 'white';
                ctx.fillRect(0, 0, size, size);
                ctx.drawImage(img, 0, 0, size, size);
            };
            img.onerror = function() {
                // Fallback: draw placeholder with URL text
                ctx.fillStyle = 'white';
                ctx.fillRect(0, 0, size, size);
                ctx.fillStyle = '#333';
                ctx.font = '12px monospace';
                ctx.textAlign = 'center';
                ctx.fillText('QR Code', size/2, size/2 - 10);
                ctx.fillText('(scan URL below)', size/2, size/2 + 10);
            };
            img.src = 'https://chart.googleapis.com/chart?cht=qr&chs=' + size + 'x' + size + '&chl=' + encodeURIComponent(text) + '&choe=UTF-8';
        }

        // ===== WEBHOOK TESTER =====

        function showWebhookTester(url) {
            document.getElementById('webhook-url').value = url || '';
            document.getElementById('webhook-headers').value = '{"Content-Type": "application/json"}';
            document.getElementById('webhook-body').value = '';
            document.getElementById('webhook-response').classList.remove('visible');
            document.getElementById('webhook-modal').classList.add('visible');
        }

        function closeWebhookModal(event) {
            if (event && event.target !== event.currentTarget) return;
            document.getElementById('webhook-modal').classList.remove('visible');
        }

        function sendWebhookTest() {
            const method = document.getElementById('webhook-method').value;
            const url = document.getElementById('webhook-url').value;
            const headersStr = document.getElementById('webhook-headers').value;
            const body = document.getElementById('webhook-body').value;
            const btn = document.getElementById('btn-webhook-send');
            const responseDiv = document.getElementById('webhook-response');

            if (!url) {
                alert('URL is required');
                return;
            }

            let headers = {};
            if (headersStr.trim()) {
                try {
                    headers = JSON.parse(headersStr);
                } catch (e) {
                    alert('Invalid JSON in headers');
                    return;
                }
            }

            btn.disabled = true;
            btn.textContent = 'Sending...';

            fetch('/api/webhook-test', {
                method: 'POST',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({ url, method, headers, body })
            })
            .then(r => r.json())
            .then(data => {
                btn.disabled = false;
                btn.textContent = 'Send Request';
                responseDiv.classList.add('visible');

                const statusEl = document.getElementById('webhook-response-status');
                const timeEl = document.getElementById('webhook-response-time');
                const bodyEl = document.getElementById('webhook-response-body');

                if (data.error) {
                    statusEl.textContent = 'Error: ' + data.error;
                    statusEl.className = 'webhook-response-status error';
                    bodyEl.textContent = '';
                } else {
                    statusEl.textContent = data.status_text;
                    statusEl.className = 'webhook-response-status ' + (data.status < 400 ? 'success' : 'error');
                    bodyEl.textContent = formatResponseBody(data.body, data.content_type);
                }
                timeEl.textContent = data.duration_ms + 'ms';
            })
            .catch(err => {
                btn.disabled = false;
                btn.textContent = 'Send Request';
                alert('Error: ' + err.message);
            });
        }

        function formatResponseBody(body, contentType) {
            if (!body) return '(empty response)';
            if (contentType && contentType.includes('application/json')) {
                try {
                    return JSON.stringify(JSON.parse(body), null, 2);
                } catch {
                    return body;
                }
            }
            return body;
        }

        // ===== TUNNEL MANAGEMENT =====

        function loadTunnels() {
            fetch('/api/tunnels')
                .then(r => r.json())
                .then(data => {
                    tunnelsList = data || [];
                    filterTunnels(filterQuery);
                    updateTunnelCount();
                })
                .catch(err => {
                    console.error('Failed to load tunnels:', err);
                    renderTunnelError();
                });
        }

        function updateTunnelCount() {
            document.getElementById('tunnel-count').textContent = tunnelsList.length;
            // Show filter if more than 5 tunnels
            const filterEl = document.getElementById('tunnel-filter');
            if (tunnelsList.length > 5) {
                filterEl.classList.add('visible');
            } else {
                filterEl.classList.remove('visible');
            }
        }

        function filterTunnels(query) {
            filterQuery = query.toLowerCase();
            if (!filterQuery) {
                filteredTunnels = tunnelsList;
            } else {
                filteredTunnels = tunnelsList.filter(t => {
                    const searchStr = (t.public_url || '') + (t.subdomain || '') + t.local_addr + t.type;
                    return searchStr.toLowerCase().includes(filterQuery);
                });
            }
            renderTunnels();
        }

        function renderTunnels() {
            const container = document.getElementById('tunnel-content');
            container.replaceChildren();

            if (tunnelsList.length === 0) {
                renderEmptyState(container);
                return;
            }

            if (filteredTunnels.length === 0) {
                const noMatch = document.createElement('div');
                noMatch.className = 'tunnel-empty';
                noMatch.textContent = 'No tunnels match your filter';
                container.appendChild(noMatch);
                return;
            }

            // Group by type
            const httpTunnels = filteredTunnels.filter(t => t.type === 'http');
            const tcpTunnels = filteredTunnels.filter(t => t.type === 'tcp');

            if (httpTunnels.length > 0) {
                container.appendChild(createTunnelGroup('http', httpTunnels));
            }
            if (tcpTunnels.length > 0) {
                container.appendChild(createTunnelGroup('tcp', tcpTunnels));
            }
        }

        function createTunnelGroup(type, tunnels) {
            const group = document.createElement('div');
            group.className = 'tunnel-group' + (collapsedGroups[type] ? ' collapsed' : '');
            group.dataset.type = type;

            const header = document.createElement('div');
            header.className = 'tunnel-group-header';
            header.onclick = () => toggleGroup(type);

            const chevron = document.createElement('span');
            chevron.className = 'chevron';
            chevron.textContent = '▼';
            header.appendChild(chevron);

            const label = document.createElement('span');
            label.textContent = type.toUpperCase();
            header.appendChild(label);

            const line = document.createElement('span');
            line.className = 'line';
            header.appendChild(line);

            const count = document.createElement('span');
            count.className = 'count';
            count.textContent = '(' + tunnels.length + ')';
            header.appendChild(count);

            group.appendChild(header);

            const body = document.createElement('div');
            body.className = 'tunnel-group-body';

            const tree = document.createElement('div');
            tree.className = 'tunnel-tree';

            tunnels.forEach((tunnel, index) => {
                const isLast = index === tunnels.length - 1;
                tree.appendChild(createTunnelItem(tunnel, isLast, type));
            });

            body.appendChild(tree);
            group.appendChild(body);

            return group;
        }

        function createTunnelItem(tunnel, isLast, type) {
            const item = document.createElement('div');
            item.className = 'tunnel-item';

            // Tree connector
            const connector = document.createElement('div');
            connector.className = 'tree-connector';
            const line = document.createElement('span');
            line.className = 'line';
            line.textContent = isLast ? '└─' : '├─';
            connector.appendChild(line);
            item.appendChild(connector);

            // Content
            const content = document.createElement('div');
            content.className = 'tunnel-content';

            // Main row
            const mainRow = document.createElement('div');
            mainRow.className = 'tunnel-main-row';

            const status = document.createElement('div');
            status.className = 'tunnel-status';
            status.title = 'Active';
            mainRow.appendChild(status);

            const url = document.createElement('span');
            url.className = 'tunnel-url';
            if (type === 'http') {
                url.textContent = tunnel.public_url || tunnel.subdomain + '.px.csam.dev';
            } else {
                url.textContent = tunnel.public_url || 'tcp://px.csam.dev:' + tunnel.remote_port;
            }
            url.title = url.textContent;
            mainRow.appendChild(url);

            // Actions
            const actions = document.createElement('div');
            actions.className = 'tunnel-actions';

            const copyBtn = document.createElement('button');
            copyBtn.className = 'tunnel-action-btn';
            copyBtn.textContent = '⎘';
            copyBtn.title = 'Copy URL';
            copyBtn.onclick = (e) => { e.stopPropagation(); copyTunnelUrl(tunnel, copyBtn); };
            actions.appendChild(copyBtn);

            const cmdBtn = document.createElement('button');
            cmdBtn.className = 'tunnel-action-btn';
            cmdBtn.textContent = type === 'http' ? '⧉' : '⌘';
            cmdBtn.title = type === 'http' ? 'Copy curl command' : 'Copy SSH command';
            cmdBtn.onclick = (e) => { e.stopPropagation(); copyTunnelCommand(tunnel, cmdBtn); };
            actions.appendChild(cmdBtn);

            const qrBtn = document.createElement('button');
            qrBtn.className = 'tunnel-action-btn';
            qrBtn.textContent = '⊞';
            qrBtn.title = 'Show QR code';
            qrBtn.onclick = (e) => { e.stopPropagation(); showQRCode(tunnel.public_url); };
            actions.appendChild(qrBtn);

            if (type === 'http') {
                const testBtn = document.createElement('button');
                testBtn.className = 'tunnel-action-btn';
                testBtn.textContent = '⚡';
                testBtn.title = 'Test webhook';
                testBtn.onclick = (e) => { e.stopPropagation(); showWebhookTester(tunnel.public_url); };
                actions.appendChild(testBtn);
            }

            const removeBtn = document.createElement('button');
            removeBtn.className = 'tunnel-action-btn remove';
            removeBtn.textContent = '✕';
            removeBtn.title = 'Remove tunnel';
            removeBtn.onclick = (e) => { e.stopPropagation(); removeTunnel(tunnel); };
            actions.appendChild(removeBtn);

            mainRow.appendChild(actions);
            content.appendChild(mainRow);

            // Local row
            const localRow = document.createElement('div');
            localRow.className = 'tunnel-local-row';

            const elbow = document.createElement('span');
            elbow.className = 'tree-elbow';
            elbow.textContent = '└─';
            localRow.appendChild(elbow);

            const arrow = document.createElement('span');
            arrow.className = 'tunnel-arrow';
            arrow.textContent = '→';
            localRow.appendChild(arrow);

            const local = document.createElement('span');
            local.className = 'tunnel-local';
            local.textContent = tunnel.local_addr;
            localRow.appendChild(local);

            content.appendChild(localRow);
            item.appendChild(content);

            return item;
        }

        function renderEmptyState(container) {
            const empty = document.createElement('div');
            empty.className = 'tunnel-empty';

            const icon = document.createElement('div');
            icon.className = 'tunnel-empty-icon';
            icon.textContent = '🚇';
            empty.appendChild(icon);

            const title = document.createElement('div');
            title.className = 'tunnel-empty-title';
            title.textContent = 'No tunnels configured';
            empty.appendChild(title);

            const hint = document.createElement('div');
            hint.className = 'tunnel-empty-hint';
            hint.textContent = 'Press N or click below to add one';
            empty.appendChild(hint);

            const btn = document.createElement('button');
            btn.className = 'btn-empty-add';
            btn.textContent = '+ Add Tunnel';
            btn.onclick = showAddTunnelModal;
            empty.appendChild(btn);

            container.appendChild(empty);
        }

        function renderTunnelError() {
            const container = document.getElementById('tunnel-content');
            container.replaceChildren();
            const err = document.createElement('div');
            err.className = 'tunnel-empty';
            err.textContent = 'Failed to load tunnels';
            container.appendChild(err);
        }

        function toggleTunnelPanel() {
            document.getElementById('tunnel-panel').classList.toggle('collapsed');
        }

        function toggleGroup(type) {
            collapsedGroups[type] = !collapsedGroups[type];
            const group = document.querySelector('.tunnel-group[data-type="' + type + '"]');
            if (group) {
                group.classList.toggle('collapsed');
            }
        }

        function copyTunnelUrl(tunnel, btn) {
            const url = tunnel.public_url || (tunnel.type === 'http' ?
                'https://' + tunnel.subdomain + '.px.csam.dev' :
                'px.csam.dev:' + tunnel.remote_port);

            navigator.clipboard.writeText(url).then(() => {
                btn.classList.add('copy-success');
                btn.textContent = '✓';
                setTimeout(() => {
                    btn.classList.remove('copy-success');
                    btn.textContent = '⎘';
                }, 1500);
            });
        }

        function copyTunnelCommand(tunnel, btn) {
            let cmd;
            if (tunnel.type === 'http') {
                const url = tunnel.public_url || 'https://' + tunnel.subdomain + '.px.csam.dev';
                cmd = 'curl ' + url;
            } else {
                const parts = (tunnel.public_url || 'px.csam.dev:' + tunnel.remote_port).replace('tcp://', '').split(':');
                cmd = 'ssh -p ' + parts[1] + ' user@' + parts[0];
            }

            navigator.clipboard.writeText(cmd).then(() => {
                btn.classList.add('copy-success');
                btn.textContent = '✓';
                setTimeout(() => {
                    btn.classList.remove('copy-success');
                    btn.textContent = tunnel.type === 'http' ? '⧉' : '⌘';
                }, 1500);
            });
        }

        function showAddTunnelModal() {
            document.getElementById('add-tunnel-modal').classList.add('visible');
            document.getElementById('tunnel-subdomain').value = '';
            document.getElementById('tunnel-port').value = '0';
            document.getElementById('tunnel-local').value = '';
            document.getElementById('tunnel-persist').checked = true;
            document.querySelector('input[name="tunnel-type"][value="http"]').checked = true;
            updateFormFields();
            updatePreview();
            document.getElementById('tunnel-local').focus();
        }

        function hideAddTunnelModal() {
            document.getElementById('add-tunnel-modal').classList.remove('visible');
        }

        function updateFormFields() {
            const type = document.querySelector('input[name="tunnel-type"]:checked').value;
            document.getElementById('type-http').classList.toggle('selected', type === 'http');
            document.getElementById('type-tcp').classList.toggle('selected', type === 'tcp');
            document.getElementById('subdomain-group').style.display = type === 'http' ? 'block' : 'none';
            document.getElementById('port-group').style.display = type === 'tcp' ? 'block' : 'none';
        }

        function updatePreview() {
            const subdomain = document.getElementById('tunnel-subdomain').value.trim();
            const preview = document.getElementById('subdomain-preview');
            if (subdomain) {
                preview.textContent = '→ ' + subdomain + '.px.csam.dev';
            } else {
                preview.textContent = '→ random.px.csam.dev';
            }
        }

        function submitAddTunnel(event) {
            event.preventDefault();

            const type = document.querySelector('input[name="tunnel-type"]:checked').value;
            const subdomain = document.getElementById('tunnel-subdomain').value.trim();
            const remotePort = parseInt(document.getElementById('tunnel-port').value) || 0;
            const localAddr = document.getElementById('tunnel-local').value.trim();
            const persist = document.getElementById('tunnel-persist').checked;

            if (!localAddr) {
                alert('Local address is required');
                return;
            }

            const btn = document.getElementById('btn-add-submit');
            btn.disabled = true;
            btn.textContent = 'Adding...';

            const body = { type, local_addr: localAddr, persist };
            if (type === 'http') {
                body.subdomain = subdomain;
            } else {
                body.remote_port = remotePort;
            }

            fetch('/api/tunnels/add', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            })
            .then(r => r.json())
            .then(data => {
                btn.disabled = false;
                btn.textContent = 'Add Tunnel ⏎';

                if (data.error) {
                    alert('Error: ' + data.error);
                } else {
                    hideAddTunnelModal();
                    loadTunnels();
                }
            })
            .catch(err => {
                btn.disabled = false;
                btn.textContent = 'Add Tunnel ⏎';
                alert('Error: ' + err.message);
            });
        }

        function removeTunnel(tunnel) {
            const name = tunnel.public_url || tunnel.subdomain || 'Port ' + tunnel.remote_port;
            if (!confirm('Remove tunnel?\\n\\n' + name + ' → ' + tunnel.local_addr)) {
                return;
            }

            const body = { type: tunnel.type, persist: true };
            if (tunnel.type === 'http') {
                body.subdomain = tunnel.subdomain;
            } else {
                body.remote_port = tunnel.remote_port;
            }

            fetch('/api/tunnels/remove', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            })
            .then(r => r.json())
            .then(data => {
                if (data.error) {
                    alert('Error: ' + data.error);
                } else {
                    loadTunnels();
                }
            })
            .catch(err => alert('Error: ' + err.message));
        }

        // ===== REQUEST LIST =====

        function loadInitial() {
            fetch('/api/requests')
                .then(r => r.json())
                .then(data => {
                    requestsList = data || [];
                    requestsList.forEach(r => requestsMap[r.id] = r);
                    renderList();
                });
        }

        function initSSE() {
            const evtSource = new EventSource('/api/events');

            evtSource.addEventListener('connected', function() {
                sseConnected = true;
                updateConnectionStatus(true);
            });

            evtSource.addEventListener('request', function(e) {
                try {
                    const req = JSON.parse(e.data);
                    addRequest(req);
                } catch (err) {
                    console.error('Failed to parse SSE:', err);
                }
            });

            evtSource.onerror = function() {
                sseConnected = false;
                updateConnectionStatus(false);
            };
        }

        function updateConnectionStatus(connected) {
            const badge = document.getElementById('traffic-badge');
            const dot = document.getElementById('connection-status');
            const statusText = document.getElementById('tunnel-status-text');

            if (connected) {
                if (!connectionStartTime) {
                    connectionStartTime = new Date();
                }
                lastHeartbeat = new Date();

                badge.classList.remove('reconnecting');
                badge.querySelector('span').textContent = 'Live';
                dot.classList.remove('disconnected');
                dot.title = getConnectionTooltip();
                statusText.textContent = '● Connected';
                statusText.style.color = 'var(--success)';
            } else {
                reconnectCount++;
                connectionStartTime = null;

                badge.classList.add('reconnecting');
                badge.querySelector('span').textContent = 'Reconnecting...';
                dot.classList.add('disconnected');
                dot.title = 'Disconnected - Reconnect #' + reconnectCount;
                statusText.textContent = '○ Reconnecting...';
                statusText.style.color = 'var(--warning)';
            }
        }

        function getConnectionTooltip() {
            let tooltip = 'Connected';
            if (lastHeartbeat) {
                const ago = Math.round((new Date() - lastHeartbeat) / 1000);
                tooltip += ' • Last heartbeat: ' + (ago < 2 ? 'just now' : ago + 's ago');
            }
            if (connectionStartTime) {
                const uptime = Math.round((new Date() - connectionStartTime) / 1000);
                tooltip += ' • Uptime: ' + formatUptime(uptime);
            }
            if (reconnectCount > 0) {
                tooltip += ' • Reconnects: ' + reconnectCount;
            }
            return tooltip;
        }

        function formatUptime(seconds) {
            if (seconds < 60) return seconds + 's';
            if (seconds < 3600) return Math.floor(seconds / 60) + 'm ' + (seconds % 60) + 's';
            const h = Math.floor(seconds / 3600);
            const m = Math.floor((seconds % 3600) / 60);
            return h + 'h ' + m + 'm';
        }

        // ===== NOTIFICATIONS =====

        async function toggleNotifications() {
            const btn = document.getElementById('btn-notify');

            if (!notificationsEnabled) {
                if ('Notification' in window) {
                    const perm = await Notification.requestPermission();
                    if (perm === 'granted') {
                        notificationsEnabled = true;
                        btn.classList.add('active');
                        btn.title = 'Disable desktop notifications';
                        showCopyToast('Notifications enabled for errors');
                    } else {
                        showCopyToast('Notification permission denied');
                    }
                } else {
                    showCopyToast('Notifications not supported');
                }
            } else {
                notificationsEnabled = false;
                btn.classList.remove('active');
                btn.title = 'Enable desktop notifications for errors';
                showCopyToast('Notifications disabled');
            }
        }

        function notifyError(req) {
            if (notificationsEnabled && req.status >= 500) {
                new Notification('Request Error', {
                    body: req.method + ' ' + req.path + ' → ' + req.status,
                    tag: req.id,
                    icon: 'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y=".9em" font-size="90">⚠️</text></svg>'
                });
            }
        }

        function exportHAR() {
            const a = document.createElement('a');
            a.href = '/api/export/har';
            a.download = 'tunnel-requests.har';
            document.body.appendChild(a);
            a.click();
            document.body.removeChild(a);
            showCopyToast('HAR file downloaded');
        }

        function addRequest(r) {
            requestsMap[r.id] = r;
            requestsList.unshift(r);
            if (requestsList.length > 100) {
                const removed = requestsList.pop();
                delete requestsMap[removed.id];
            }
            // Re-apply filters
            filterRequests();
            // Notify on errors
            notifyError(r);
        }

        function renderList() {
            // Use filtered list
            filterRequests();
        }

        function createRequestItem(r) {
            const statusClass = r.status >= 500 ? 's-5xx' : (r.status >= 400 ? 's-4xx' : 's-2xx');

            const item = document.createElement('div');
            item.className = 'req-item' + (r.id === selectedId ? ' active' : '');
            item.onclick = (e) => {
                if (e.target.type !== 'checkbox') {
                    selectRequest(r.id);
                }
            };

            const checkbox = document.createElement('input');
            checkbox.type = 'checkbox';
            checkbox.className = 'diff-checkbox';
            checkbox.checked = selectedForDiff.includes(r.id);
            checkbox.onclick = (e) => {
                e.stopPropagation();
                toggleDiffSelection(r.id, e.target.checked);
            };
            item.appendChild(checkbox);

            const dot = document.createElement('div');
            dot.className = 'req-status-dot ' + statusClass;
            item.appendChild(dot);

            const info = document.createElement('div');
            info.className = 'req-info';

            const main = document.createElement('div');
            main.className = 'req-main';

            const method = document.createElement('span');
            method.className = 'req-method ' + statusClass;
            method.textContent = r.method;
            main.appendChild(method);

            const path = document.createElement('span');
            path.className = 'req-path';
            path.textContent = r.path;
            main.appendChild(path);

            info.appendChild(main);

            const meta = document.createElement('div');
            meta.className = 'req-meta';

            const status = document.createElement('span');
            status.textContent = r.status;
            meta.appendChild(status);

            const duration = document.createElement('span');
            duration.textContent = r.duration;
            meta.appendChild(duration);

            const time = document.createElement('span');
            time.textContent = new Date(r.timestamp).toLocaleTimeString();
            meta.appendChild(time);

            info.appendChild(meta);
            item.appendChild(info);

            return item;
        }

        function selectRequest(id) {
            selectedId = id;
            const r = requestsMap[id];
            if (!r) return;

            document.getElementById('empty-state').style.display = 'none';
            document.getElementById('detail-view').classList.add('visible');

            const statusClass = r.status >= 500 ? 's-5xx' : (r.status >= 400 ? 's-4xx' : 's-2xx');

            const methodEl = document.getElementById('d-method');
            methodEl.textContent = r.method;
            methodEl.className = 'detail-method ' + statusClass;

            document.getElementById('d-path').textContent = r.path;

            const statusEl = document.getElementById('d-status');
            statusEl.textContent = r.status;
            statusEl.className = 'detail-status ' + statusClass;

            document.getElementById('d-time').textContent = new Date(r.timestamp).toLocaleString();
            document.getElementById('d-duration').textContent = r.duration;

            renderHeaders('req', r.req_header);
            renderHeaders('res', r.res_header);
            renderBody('req', r.req_body, r.req_header ? r.req_header['Content-Type'] : '');
            renderBody('res', r.res_body, r.content_type || (r.res_header ? r.res_header['Content-Type'] : ''));

            document.getElementById('replay-result').classList.remove('visible');
            renderList();
        }

        function replayRequest() {
            if (!selectedId) return;

            const btn = document.getElementById('btn-replay');
            const resultDiv = document.getElementById('replay-result');
            btn.disabled = true;
            btn.textContent = '⏳ Replaying...';
            resultDiv.classList.remove('visible');

            fetch('/api/replay/' + selectedId, { method: 'POST' })
                .then(r => r.json())
                .then(data => {
                    btn.disabled = false;
                    btn.textContent = '▶ Replay';
                    resultDiv.classList.add('visible');

                    if (data.error) {
                        resultDiv.className = 'replay-result visible error';
                        resultDiv.textContent = 'Error: ' + data.error;
                    } else {
                        resultDiv.className = 'replay-result visible success';
                        resultDiv.textContent = '✓ Replay completed: ' + data.status + ' (' + data.duration + ')';
                    }
                })
                .catch(err => {
                    btn.disabled = false;
                    btn.textContent = '▶ Replay';
                    resultDiv.classList.add('visible');
                    resultDiv.className = 'replay-result visible error';
                    resultDiv.textContent = 'Error: ' + err.message;
                });
        }

        function renderHeaders(type, headers) {
            const container = document.getElementById(type + '-headers-list');
            container.replaceChildren();

            const entries = Object.entries(headers || {});
            if (entries.length === 0) {
                const empty = document.createElement('div');
                empty.style.cssText = 'grid-column: span 2; color: var(--text-muted); font-style: italic;';
                empty.textContent = 'No headers';
                container.appendChild(empty);
                return;
            }

            entries.forEach(([key, val]) => {
                const keyDiv = document.createElement('div');
                keyDiv.className = 'kv-key';
                keyDiv.textContent = key + ':';
                container.appendChild(keyDiv);

                const valDiv = document.createElement('div');
                valDiv.className = 'kv-val';
                valDiv.textContent = val.join(', ');
                container.appendChild(valDiv);
            });
        }

        function renderBody(type, body, contentType) {
            const el = document.getElementById(type + '-body-content');
            if (!body) {
                el.innerHTML = '<span style="color: var(--text-muted)">No content</span>';
                return;
            }

            const parts = body.split('\\r\\n\\r\\n');
            let content = parts.length > 1 ? parts.slice(1).join('\\r\\n\\r\\n') : body;

            // Apply syntax highlighting
            el.innerHTML = formatBody(content || body, contentType || '');
        }

        function switchTab(section, tabName) {
            const headers = document.getElementById(section + '-headers');
            const body = document.getElementById(section + '-body');
            const tabs = event.target.parentNode;

            Array.from(tabs.children).forEach(t => t.classList.remove('active'));
            event.target.classList.add('active');

            headers.classList.toggle('active', tabName === 'headers');
            body.classList.toggle('active', tabName === 'body');
        }

        // ===== KEYBOARD SHORTCUTS =====

        document.addEventListener('keydown', function(e) {
            // Don't trigger shortcuts when typing in inputs
            if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') {
                if (e.key === 'Escape') {
                    e.target.blur();
                    hideAddTunnelModal();
                }
                return;
            }

            // N - New tunnel
            if (e.key === 'n' || e.key === 'N') {
                e.preventDefault();
                showAddTunnelModal();
            }

            // Ctrl+K or Cmd+K - Focus filter
            if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
                e.preventDefault();
                const filter = document.getElementById('tunnel-filter-input');
                if (tunnelsList.length > 5) {
                    filter.focus();
                }
            }

            // Escape - Close modal
            if (e.key === 'Escape') {
                hideAddTunnelModal();
            }
        });

        // Initialize
        loadTunnels();
        loadInitial();
        initSSE();
        fetchMetrics();

        // Update connection tooltip every 5 seconds
        setInterval(function() {
            if (sseConnected) {
                const dot = document.getElementById('connection-status');
                if (dot) dot.title = getConnectionTooltip();
            }
        }, 5000);
    </script>
</body>
</html>
`
