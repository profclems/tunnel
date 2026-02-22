package client

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

//go:embed dashboard.html
var dashboardHTML string

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
	P50LatencyMs   int64          `json:"p50_latency_ms"`
	P95LatencyMs   int64          `json:"p95_latency_ms"`
	P99LatencyMs   int64          `json:"p99_latency_ms"`
	ErrorsByStatus map[int]int64  `json:"errors_by_status"`
	LatencyBuckets []int64        `json:"latency_buckets"` // Response time histogram
}

// MockRule defines a mock response rule
type MockRule struct {
	ID         string            `json:"id"`
	Pattern    string            `json:"pattern"` // Glob pattern: /api/*, /users/:id
	Method     string            `json:"method"`  // GET, POST, or * for any
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	DelayMs    int               `json:"delay_ms"` // Simulate latency
	Active     bool              `json:"active"`
	HitCount   int64             `json:"hit_count"`
	CreatedAt  time.Time         `json:"created_at"`
}

// SharedRequest represents a shareable captured request
type SharedRequest struct {
	Token     string         `json:"token"`
	Request   *RequestRecord `json:"request"`
	ExpiresAt time.Time      `json:"expires_at"`
}

// WSMessage represents a WebSocket message for inspection
type WSMessage struct {
	ID          string    `json:"id"`
	TunnelID    string    `json:"tunnel_id"`
	Direction   string    `json:"direction"`    // "in" (client->server) or "out" (server->client)
	MessageType string    `json:"message_type"` // "text" or "binary"
	Data        string    `json:"data"`
	Size        int64     `json:"size"`
	Timestamp   time.Time `json:"timestamp"`
}

// WSConnection represents an active WebSocket connection
type WSConnection struct {
	ID           string    `json:"id"`
	TunnelID     string    `json:"tunnel_id"`
	Path         string    `json:"path"`
	StartedAt    time.Time `json:"started_at"`
	MessageCount int64     `json:"message_count"`
	BytesIn      int64     `json:"bytes_in"`
	BytesOut     int64     `json:"bytes_out"`
	Active       bool      `json:"active"`
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

	// Mock rules
	mockRules   []*MockRule
	mockRulesMu sync.RWMutex

	// Shared requests
	sharedRequests   map[string]*SharedRequest
	sharedRequestsMu sync.RWMutex

	// WebSocket tracking
	wsMessages      []*WSMessage
	wsConnections   map[string]*WSConnection
	wsMessagesMu    sync.RWMutex
	wsConnectionsMu sync.RWMutex
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
		requests:       make([]*RequestRecord, 0),
		maxSize:        100, // Keep last 100 requests
		broadcast:      make(chan *RequestRecord, 10),
		sseClients:     make(map[chan *RequestRecord]struct{}),
		mockRules:      make([]*MockRule, 0),
		sharedRequests: make(map[string]*SharedRequest),
		wsMessages:     make([]*WSMessage, 0),
		wsConnections:  make(map[string]*WSConnection),
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

// findMatchingMock finds a mock rule that matches the request
func (i *Inspector) findMatchingMock(method, path string) *MockRule {
	i.mockRulesMu.RLock()
	defer i.mockRulesMu.RUnlock()

	for _, mock := range i.mockRules {
		if !mock.Active {
			continue
		}
		// Check method match
		if mock.Method != "*" && mock.Method != "" && mock.Method != method {
			continue
		}
		// Check pattern match (glob-style)
		if matchPattern(mock.Pattern, path) {
			return mock
		}
	}
	return nil
}

// matchPattern matches a glob-style pattern against a path
func matchPattern(pattern, path string) bool {
	// Simple glob matching: * matches any segment, ** matches any segments
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")

	pi := 0 // pattern index
	for _, pp := range patternParts {
		if pp == "**" {
			// ** matches any remaining segments
			return true
		}
		if pi >= len(pathParts) {
			return false
		}
		if pp == "*" {
			// * matches any single segment
			pi++
			continue
		}
		if strings.HasPrefix(pp, ":") {
			// :param matches any single segment (path parameter)
			pi++
			continue
		}
		if pp != pathParts[pi] {
			return false
		}
		pi++
	}
	return pi == len(pathParts)
}

// serveMockResponse writes a mock response and records it
func (i *Inspector) serveMockResponse(stream net.Conn, rec *RequestRecord, mock *MockRule) {
	// Simulate delay if configured
	if mock.DelayMs > 0 {
		time.Sleep(time.Duration(mock.DelayMs) * time.Millisecond)
	}

	// Increment hit count
	i.mockRulesMu.Lock()
	mock.HitCount++
	i.mockRulesMu.Unlock()

	// Build response
	duration := time.Since(rec.Timestamp)
	rec.Status = mock.StatusCode
	rec.Duration = duration.String()
	rec.DurationMs = duration.Milliseconds()
	rec.ResBody = mock.Body
	rec.ResBodySize = int64(len(mock.Body))
	rec.ContentType = mock.Headers["Content-Type"]
	rec.ResHeader = make(http.Header)
	for k, v := range mock.Headers {
		rec.ResHeader.Set(k, v)
	}
	rec.ResHeader.Set("X-Mock-ID", mock.ID)

	// Record the mocked request
	i.addRecord(rec)
	i.recordMetrics(rec)

	// Send response to client
	resp := &http.Response{
		StatusCode: mock.StatusCode,
		Status:     fmt.Sprintf("%d %s", mock.StatusCode, http.StatusText(mock.StatusCode)),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     rec.ResHeader.Clone(),
		Body:       io.NopCloser(strings.NewReader(mock.Body)),
	}
	if resp.Header.Get("Content-Type") == "" {
		resp.Header.Set("Content-Type", "application/json")
	}
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(mock.Body)))
	resp.Write(stream)
}

// WebSocket tracking methods

// TrackWSConnection registers a new WebSocket connection
func (i *Inspector) TrackWSConnection(tunnelID, path string) string {
	i.wsConnectionsMu.Lock()
	defer i.wsConnectionsMu.Unlock()

	id := uuid.New().String()[:8]
	conn := &WSConnection{
		ID:        id,
		TunnelID:  tunnelID,
		Path:      path,
		StartedAt: time.Now(),
		Active:    true,
	}
	i.wsConnections[id] = conn
	return id
}

// CloseWSConnection marks a WebSocket connection as closed
func (i *Inspector) CloseWSConnection(connID string) {
	i.wsConnectionsMu.Lock()
	defer i.wsConnectionsMu.Unlock()

	if conn, exists := i.wsConnections[connID]; exists {
		conn.Active = false
	}
}

// TrackWSMessage records a WebSocket message
func (i *Inspector) TrackWSMessage(connID, direction, msgType string, data []byte) {
	i.wsMessagesMu.Lock()
	defer i.wsMessagesMu.Unlock()

	// Update connection stats
	i.wsConnectionsMu.Lock()
	if conn, exists := i.wsConnections[connID]; exists {
		conn.MessageCount++
		if direction == "in" {
			conn.BytesIn += int64(len(data))
		} else {
			conn.BytesOut += int64(len(data))
		}
	}
	i.wsConnectionsMu.Unlock()

	// Truncate data for display if too large
	dataStr := string(data)
	if len(dataStr) > 4096 {
		dataStr = dataStr[:4096] + "... (truncated)"
	}

	msg := &WSMessage{
		ID:          uuid.New().String()[:8],
		TunnelID:    connID,
		Direction:   direction,
		MessageType: msgType,
		Data:        dataStr,
		Size:        int64(len(data)),
		Timestamp:   time.Now(),
	}

	i.wsMessages = append(i.wsMessages, msg)
	if len(i.wsMessages) > 200 {
		i.wsMessages = i.wsMessages[1:]
	}
}

// GetWSMessages returns WebSocket messages, optionally filtered by connection
func (i *Inspector) GetWSMessages(connID string) []*WSMessage {
	i.wsMessagesMu.RLock()
	defer i.wsMessagesMu.RUnlock()

	if connID == "" {
		result := make([]*WSMessage, len(i.wsMessages))
		copy(result, i.wsMessages)
		return result
	}

	var result []*WSMessage
	for _, msg := range i.wsMessages {
		if msg.TunnelID == connID {
			result = append(result, msg)
		}
	}
	return result
}

// GetWSConnections returns all WebSocket connections
func (i *Inspector) GetWSConnections() []*WSConnection {
	i.wsConnectionsMu.RLock()
	defer i.wsConnectionsMu.RUnlock()

	result := make([]*WSConnection, 0, len(i.wsConnections))
	for _, conn := range i.wsConnections {
		result = append(result, conn)
	}
	return result
}

// ClearWSMessages clears all WebSocket messages
func (i *Inspector) ClearWSMessages() {
	i.wsMessagesMu.Lock()
	defer i.wsMessagesMu.Unlock()
	i.wsMessages = make([]*WSMessage, 0)
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

	// Calculate average latency and percentiles
	var avgLatency, p50, p95, p99 int64
	// Latency histogram buckets: <10ms, 10-25ms, 25-50ms, 50-100ms, 100-250ms, 250-500ms, 500ms-1s, 1-2.5s, 2.5-5s, >5s
	latencyBuckets := make([]int64, 10)
	bucketLimits := []int64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

	if len(latency) > 0 {
		var total int64
		sorted := make([]int64, len(latency))
		for idx, p := range latency {
			total += p.LatencyMs
			sorted[idx] = p.LatencyMs

			// Assign to histogram bucket
			bucketIdx := len(bucketLimits) // Default to last bucket (>5s)
			for bi, limit := range bucketLimits {
				if p.LatencyMs < limit {
					bucketIdx = bi
					break
				}
			}
			latencyBuckets[bucketIdx]++
		}
		avgLatency = total / int64(len(latency))

		// Sort for percentile calculation
		sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })

		// Calculate percentiles
		n := len(sorted)
		p50 = sorted[n*50/100]
		p95 = sorted[n*95/100]
		if n > 1 {
			p99 = sorted[n*99/100]
		} else {
			p99 = sorted[n-1]
		}
	}

	// Calculate requests per minute
	var totalRate int
	for _, r := range rateHistory {
		totalRate += r
	}
	reqPerMin := float64(totalRate)

	// Count errors by status code
	i.mu.RLock()
	errorsByStatus := make(map[int]int64)
	for _, rec := range i.requests {
		if rec.Status >= 400 {
			errorsByStatus[rec.Status]++
		}
	}
	i.mu.RUnlock()

	return MetricsSnapshot{
		LatencyHistory: latency,
		RateHistory:    rateHistory,
		TotalBytesIn:   i.metrics.TotalBytesIn,
		TotalBytesOut:  i.metrics.TotalBytesOut,
		TotalRequests:  i.metrics.TotalRequests,
		ErrorCount:     i.metrics.ErrorCount,
		AvgLatencyMs:   avgLatency,
		RequestsPerMin: reqPerMin,
		P50LatencyMs:   p50,
		P95LatencyMs:   p95,
		P99LatencyMs:   p99,
		ErrorsByStatus: errorsByStatus,
		LatencyBuckets: latencyBuckets,
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

	// Check for matching mock rule FIRST
	if mock := i.findMatchingMock(req.Method, req.URL.Path); mock != nil {
		i.serveMockResponse(stream, rec, mock)
		return
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

	// ========== MOCK RULES API ==========
	mux.HandleFunc("/api/mocks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method == http.MethodGet {
			i.mockRulesMu.RLock()
			rules := make([]*MockRule, len(i.mockRules))
			copy(rules, i.mockRules)
			i.mockRulesMu.RUnlock()
			json.NewEncoder(w).Encode(rules)
			return
		}

		if r.Method == http.MethodPost {
			var rule MockRule
			if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
				return
			}
			rule.ID = uuid.New().String()[:8]
			rule.CreatedAt = time.Now()
			rule.Active = true

			i.mockRulesMu.Lock()
			i.mockRules = append(i.mockRules, &rule)
			i.mockRulesMu.Unlock()

			json.NewEncoder(w).Encode(rule)
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	mux.HandleFunc("/api/mocks/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, DELETE, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Extract mock ID and action from path: /api/mocks/{id} or /api/mocks/{id}/toggle
		path := strings.TrimPrefix(r.URL.Path, "/api/mocks/")
		parts := strings.Split(path, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, "Mock ID required", http.StatusBadRequest)
			return
		}
		mockID := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}

		i.mockRulesMu.Lock()
		defer i.mockRulesMu.Unlock()

		// Find mock by ID
		var mockIdx = -1
		for idx, m := range i.mockRules {
			if m.ID == mockID {
				mockIdx = idx
				break
			}
		}
		if mockIdx == -1 {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "Mock not found"})
			return
		}

		if action == "toggle" && r.Method == http.MethodPost {
			i.mockRules[mockIdx].Active = !i.mockRules[mockIdx].Active
			json.NewEncoder(w).Encode(i.mockRules[mockIdx])
			return
		}

		if r.Method == http.MethodPut {
			var rule MockRule
			if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
				return
			}
			rule.ID = mockID
			rule.CreatedAt = i.mockRules[mockIdx].CreatedAt
			rule.HitCount = i.mockRules[mockIdx].HitCount
			i.mockRules[mockIdx] = &rule
			json.NewEncoder(w).Encode(rule)
			return
		}

		if r.Method == http.MethodDelete {
			i.mockRules = append(i.mockRules[:mockIdx], i.mockRules[mockIdx+1:]...)
			json.NewEncoder(w).Encode(map[string]string{"success": "true"})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	// ========== SHARE REQUEST API ==========
	mux.HandleFunc("/api/share", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			RequestID string `json:"request_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
			return
		}

		rec := i.GetRequestByID(req.RequestID)
		if rec == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "Request not found"})
			return
		}

		token := uuid.New().String()[:16]
		shared := &SharedRequest{
			Token:     token,
			Request:   rec,
			ExpiresAt: time.Now().Add(24 * time.Hour),
		}

		i.sharedRequestsMu.Lock()
		i.sharedRequests[token] = shared
		i.sharedRequestsMu.Unlock()

		json.NewEncoder(w).Encode(map[string]string{
			"token":      token,
			"expires_at": shared.ExpiresAt.Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/api/shared/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		token := strings.TrimPrefix(r.URL.Path, "/api/shared/")
		if token == "" {
			http.Error(w, "Token required", http.StatusBadRequest)
			return
		}

		i.sharedRequestsMu.RLock()
		shared, exists := i.sharedRequests[token]
		i.sharedRequestsMu.RUnlock()

		if !exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "Shared request not found or expired"})
			return
		}

		if time.Now().After(shared.ExpiresAt) {
			i.sharedRequestsMu.Lock()
			delete(i.sharedRequests, token)
			i.sharedRequestsMu.Unlock()
			w.WriteHeader(http.StatusGone)
			json.NewEncoder(w).Encode(map[string]string{"error": "Shared request has expired"})
			return
		}

		json.NewEncoder(w).Encode(shared)
	})

	// ========== WEBSOCKET INSPECTOR API ==========
	mux.HandleFunc("/api/ws-connections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		connections := i.GetWSConnections()
		json.NewEncoder(w).Encode(connections)
	})

	mux.HandleFunc("/api/ws-messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		connID := r.URL.Query().Get("connection_id")
		messages := i.GetWSMessages(connID)
		json.NewEncoder(w).Encode(messages)
	})

	mux.HandleFunc("/api/ws-messages/clear", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		i.ClearWSMessages()
		json.NewEncoder(w).Encode(map[string]string{"success": "true"})
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
