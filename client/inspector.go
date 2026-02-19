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
}

func NewInspector() *Inspector {
	return &Inspector{
		requests:   make([]*RequestRecord, 0),
		maxSize:    100, // Keep last 100 requests
		broadcast:  make(chan *RequestRecord, 10),
		sseClients: make(map[chan *RequestRecord]struct{}),
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

	rec := &RequestRecord{
		ID:        id,
		Method:    req.Method,
		Path:      req.URL.Path,
		Host:      req.Host,
		URL:       urlStr,
		Timestamp: start,
		ReqHeader: req.Header.Clone(),
		ReqBody:   reqBody,
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
		rec.Status = http.StatusBadGateway
		rec.Duration = time.Since(start).String()
		i.addRecord(rec)

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

	rec.Status = resp.StatusCode
	rec.ResHeader = resp.Header.Clone()
	rec.ResBody = resBody
	rec.Duration = time.Since(start).String()

	// Save Record
	i.addRecord(rec)

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
            --bg: #f9fafb;
            --border: #e5e7eb;
            --sidebar-bg: #ffffff;
            --details-bg: #ffffff;
            --text-main: #1f2937;
            --text-sub: #6b7280;
            --primary: #2563eb;
            --success: #059669;
            --error: #dc2626;
            --warning: #d97706;
            --code-bg: #f3f4f6;
        }
        * { box-sizing: border-box; }
        body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background: var(--bg); color: var(--text-main); height: 100vh; display: flex; overflow: hidden; }
        
        /* Layout */
        .sidebar { width: 400px; background: var(--sidebar-bg); border-right: 1px solid var(--border); display: flex; flex-direction: column; }
        .main { flex: 1; background: var(--details-bg); display: flex; flex-direction: column; overflow: hidden; }
        
        /* Sidebar Header */
        .sidebar-header { padding: 16px; border-bottom: 1px solid var(--border); display: flex; justify-content: space-between; align-items: center; }
        .sidebar-header h1 { font-size: 16px; margin: 0; font-weight: 600; }
        .status-badge { font-size: 12px; padding: 2px 8px; background: #dbeafe; color: var(--primary); border-radius: 12px; }

        /* Request List */
        .req-list { flex: 1; overflow-y: auto; }
        .req-item { padding: 12px 16px; border-bottom: 1px solid var(--border); cursor: pointer; transition: background 0.1s; display: flex; align-items: center; gap: 10px; }
        .req-item:hover { background: #f9fafb; }
        .req-item.active { background: #eff6ff; border-left: 3px solid var(--primary); }
        
        .req-method { font-size: 11px; font-weight: 700; width: 40px; text-transform: uppercase; }
        .req-info { flex: 1; min-width: 0; }
        .req-path { font-size: 13px; font-family: 'Menlo', 'Monaco', monospace; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; margin-bottom: 4px; }
        .req-meta { font-size: 11px; color: var(--text-sub); display: flex; gap: 8px; }
        
        .status-dot { width: 8px; height: 8px; border-radius: 50%; }
        .s-2xx { background: var(--success); color: var(--success); }
        .s-4xx { background: var(--warning); color: var(--warning); }
        .s-5xx { background: var(--error); color: var(--error); }

        /* Detail View */
        .empty-state { flex: 1; display: flex; align-items: center; justify-content: center; color: var(--text-sub); font-size: 14px; }
        .detail-view { display: none; flex-direction: column; height: 100%; }
        .detail-view.visible { display: flex; }
        
        .detail-header { padding: 16px 24px; border-bottom: 1px solid var(--border); background: #fff; }
        .detail-title { font-size: 18px; font-weight: 600; margin-bottom: 8px; display: flex; align-items: center; gap: 12px; }
        .detail-meta { font-size: 13px; color: var(--text-sub); display: flex; gap: 16px; }
        
        .detail-content { flex: 1; overflow-y: auto; padding: 24px; display: flex; flex-direction: column; gap: 24px; }
        
        .section-title { font-size: 12px; text-transform: uppercase; letter-spacing: 0.5px; color: var(--text-sub); font-weight: 600; margin-bottom: 8px; border-bottom: 1px solid var(--border); padding-bottom: 4px; }
        
        .kv-grid { display: grid; grid-template-columns: auto 1fr; gap: 8px 16px; font-size: 13px; margin-bottom: 16px; }
        .kv-key { font-weight: 500; color: var(--text-sub); text-align: right; }
        .kv-val { font-family: 'Menlo', monospace; word-break: break-all; }

        pre { margin: 0; background: var(--code-bg); padding: 16px; border-radius: 6px; font-size: 12px; overflow-x: auto; border: 1px solid var(--border); white-space: pre-wrap; word-break: break-all; }
        
        /* Utility */
        .tabs { display: flex; border-bottom: 1px solid var(--border); margin-bottom: 16px; }
        .tab { padding: 8px 16px; cursor: pointer; font-size: 13px; font-weight: 500; color: var(--text-sub); border-bottom: 2px solid transparent; }
        .tab.active { color: var(--primary); border-bottom-color: var(--primary); }
        .tab-content { display: none; }
        .tab-content.active { display: block; }

        /* Replay Button */
        .btn-replay { padding: 6px 12px; background: var(--primary); color: white; border: none; border-radius: 4px; cursor: pointer; font-size: 12px; font-weight: 500; }
        .btn-replay:hover { background: #1d4ed8; }
        .btn-replay:disabled { background: #9ca3af; cursor: not-allowed; }
        .replay-result { margin-top: 16px; padding: 12px; background: #f0f9ff; border: 1px solid #bae6fd; border-radius: 6px; }
        .replay-result.error { background: #fef2f2; border-color: #fecaca; }

    </style>
</head>
<body>
    <div class="sidebar">
        <div class="sidebar-header">
            <h1>Tunnel Traffic</h1>
            <span class="status-badge">Live</span>
        </div>
        <div id="req-list" class="req-list">
            <!-- Items injected here -->
        </div>
    </div>

    <div class="main">
        <div id="empty-state" class="empty-state">Select a request to inspect details</div>
        
        <div id="detail-view" class="detail-view">
            <div class="detail-header">
                <div class="detail-title">
                    <span id="d-method" class="req-method">GET</span>
                    <span id="d-path" style="font-family: monospace;">/path</span>
                </div>
                <div class="detail-meta">
                    <span id="d-status" class="s-2xx" style="font-weight: 600;">200 OK</span>
                    <span id="d-time">12:00:00</span>
                    <span id="d-duration">120ms</span>
                    <button id="btn-replay" class="btn-replay" onclick="replayRequest()">Replay</button>
                </div>
                <div id="replay-result" class="replay-result" style="display: none;"></div>
            </div>

            <div class="detail-content">
                
                <!-- Request Section -->
                <div>
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

                <!-- Response Section -->
                <div>
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

    <script>
        // Note: This dashboard is local-only (localhost) and data comes from our own inspector.
        // DOM manipulation uses textContent for untrusted content where possible.
        let requestsMap = {};
        let requestsList = [];
        let selectedId = null;
        let sseConnected = false;

        // Helper to escape HTML for safe display
        function escapeHtml(text) {
            const div = document.createElement('div');
            div.textContent = text;
            return div.innerHTML;
        }

        // Create a request item element using safe DOM methods
        function createRequestItem(r) {
            const statusClass = r.status >= 500 ? 's-5xx' : (r.status >= 400 ? 's-4xx' : 's-2xx');
            const activeClass = r.id === selectedId ? ' active' : '';

            const item = document.createElement('div');
            item.className = 'req-item' + activeClass;
            item.onclick = () => selectRequest(r.id);

            const dot = document.createElement('div');
            dot.className = 'status-dot ' + statusClass;
            item.appendChild(dot);

            const info = document.createElement('div');
            info.className = 'req-info';

            const pathDiv = document.createElement('div');
            pathDiv.className = 'req-path';
            const methodSpan = document.createElement('span');
            methodSpan.className = 'req-method ' + statusClass;
            methodSpan.textContent = r.method;
            pathDiv.appendChild(methodSpan);
            pathDiv.appendChild(document.createTextNode(' ' + r.path));
            info.appendChild(pathDiv);

            const metaDiv = document.createElement('div');
            metaDiv.className = 'req-meta';
            const statusSpan = document.createElement('span');
            statusSpan.textContent = r.status;
            const durationSpan = document.createElement('span');
            durationSpan.textContent = r.duration;
            const timeSpan = document.createElement('span');
            timeSpan.textContent = new Date(r.timestamp).toLocaleTimeString();
            metaDiv.appendChild(statusSpan);
            metaDiv.appendChild(durationSpan);
            metaDiv.appendChild(timeSpan);
            info.appendChild(metaDiv);

            item.appendChild(info);
            return item;
        }

        // Render the request list using safe DOM methods
        function renderList() {
            const list = document.getElementById('req-list');
            list.replaceChildren(); // Clear existing content

            requestsList.forEach(r => {
                list.appendChild(createRequestItem(r));
            });
        }

        // Add a new request to the list (from SSE)
        function addRequest(r) {
            requestsMap[r.id] = r;
            requestsList.unshift(r);
            if (requestsList.length > 100) {
                const removed = requestsList.pop();
                delete requestsMap[removed.id];
            }
            renderList();
        }

        // Load initial requests via REST API
        function loadInitial() {
            fetch('/api/requests').then(r => r.json()).then(data => {
                requestsList = data || [];
                requestsList.forEach(r => {
                    requestsMap[r.id] = r;
                });
                renderList();
            });
        }

        // Initialize SSE connection for real-time updates
        function initSSE() {
            const evtSource = new EventSource('/api/events');

            evtSource.addEventListener('connected', function(e) {
                sseConnected = true;
                const badge = document.querySelector('.status-badge');
                badge.textContent = 'Live';
                badge.style.background = '#dcfce7';
                badge.style.color = '#059669';
            });

            evtSource.addEventListener('request', function(e) {
                try {
                    const req = JSON.parse(e.data);
                    addRequest(req);
                } catch (err) {
                    console.error('Failed to parse SSE data:', err);
                }
            });

            evtSource.onerror = function(e) {
                sseConnected = false;
                const badge = document.querySelector('.status-badge');
                badge.textContent = 'Reconnecting...';
                badge.style.background = '#fef3c7';
                badge.style.color = '#d97706';
            };
        }

        function selectRequest(id) {
            selectedId = id;
            const r = requestsMap[id];
            if (!r) return;

            document.getElementById('empty-state').style.display = 'none';
            document.getElementById('detail-view').classList.add('visible');

            document.getElementById('d-method').textContent = r.method;
            document.getElementById('d-path').textContent = r.path;
            const statusEl = document.getElementById('d-status');
            statusEl.textContent = r.status;
            statusEl.className = r.status >= 500 ? 's-5xx' : (r.status >= 400 ? 's-4xx' : 's-2xx');
            statusEl.style.fontWeight = 'bold';

            document.getElementById('d-time').textContent = new Date(r.timestamp).toLocaleString();
            document.getElementById('d-duration').textContent = r.duration;

            renderHeaders('req', r.req_header);
            renderHeaders('res', r.res_header);
            renderBody('req', r.req_body);
            renderBody('res', r.res_body);

            // Hide previous replay result
            document.getElementById('replay-result').style.display = 'none';

            renderList();
        }

        function replayRequest() {
            if (!selectedId) return;

            const btn = document.getElementById('btn-replay');
            const resultDiv = document.getElementById('replay-result');
            btn.disabled = true;
            btn.textContent = 'Replaying...';
            resultDiv.style.display = 'none';

            fetch('/api/replay/' + selectedId, { method: 'POST' })
                .then(r => r.json())
                .then(data => {
                    btn.disabled = false;
                    btn.textContent = 'Replay';
                    resultDiv.style.display = 'block';

                    if (data.error) {
                        resultDiv.className = 'replay-result error';
                        resultDiv.textContent = 'Error: ' + data.error;
                    } else {
                        resultDiv.className = 'replay-result';
                        resultDiv.textContent = 'Replay completed: ' + data.status + ' (' + data.duration + ')';
                    }
                })
                .catch(err => {
                    btn.disabled = false;
                    btn.textContent = 'Replay';
                    resultDiv.style.display = 'block';
                    resultDiv.className = 'replay-result error';
                    resultDiv.textContent = 'Error: ' + err.message;
                });
        }

        function renderHeaders(type, headers) {
            const container = document.getElementById(type + '-headers-list');
            container.replaceChildren();

            const entries = Object.entries(headers || {});
            if (entries.length === 0) {
                const noHeaders = document.createElement('div');
                noHeaders.style.gridColumn = 'span 2';
                noHeaders.style.color = '#999';
                noHeaders.style.fontStyle = 'italic';
                noHeaders.textContent = 'No headers';
                container.appendChild(noHeaders);
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

        function renderBody(type, body) {
            const el = document.getElementById(type + '-body-content');
            if (!body) {
                el.textContent = 'No content';
                return;
            }

            const parts = body.split('\r\n\r\n');
            let content = parts.length > 1 ? parts.slice(1).join('\r\n\r\n') : body;

            try {
                const json = JSON.parse(content);
                el.textContent = JSON.stringify(json, null, 2);
            } catch (e) {
                el.textContent = content || body;
            }
        }

        function switchTab(section, tabName) {
            const headerContent = document.getElementById(section + '-headers');
            const bodyContent = document.getElementById(section + '-body');

            const clickedTab = event.target;
            const tabsContainer = clickedTab.parentNode;

            Array.from(tabsContainer.children).forEach(t => t.classList.remove('active'));
            clickedTab.classList.add('active');

            if (tabName === 'headers') {
                headerContent.classList.add('active');
                bodyContent.classList.remove('active');
            } else {
                headerContent.classList.remove('active');
                bodyContent.classList.add('active');
            }
        }

        loadInitial();
        initSSE();
    </script>
</body>
</html>
`
