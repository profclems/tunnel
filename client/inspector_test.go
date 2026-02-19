package client

import (
	"net/http"
	"testing"
	"time"
)

func TestNewInspector(t *testing.T) {
	inspector := NewInspector()

	if inspector == nil {
		t.Fatal("NewInspector returned nil")
	}

	if inspector.maxSize != 100 {
		t.Errorf("maxSize = %d, want 100", inspector.maxSize)
	}

	if inspector.requests == nil {
		t.Error("requests slice is nil")
	}

	if inspector.broadcast == nil {
		t.Error("broadcast channel is nil")
	}

	if inspector.sseClients == nil {
		t.Error("sseClients map is nil")
	}
}

func TestInspector_AddRecord(t *testing.T) {
	inspector := NewInspector()

	rec := &RequestRecord{
		ID:        "test-id-1",
		Method:    "GET",
		Path:      "/api/test",
		Status:    200,
		Duration:  "10ms",
		Timestamp: time.Now(),
	}

	inspector.addRecord(rec)

	recent := inspector.GetRecent()
	if len(recent) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recent))
	}

	if recent[0].ID != rec.ID {
		t.Errorf("ID = %s, want %s", recent[0].ID, rec.ID)
	}
}

func TestInspector_GetRecent_Order(t *testing.T) {
	inspector := NewInspector()

	// Add multiple records
	for i := 0; i < 5; i++ {
		rec := &RequestRecord{
			ID:        string(rune('a' + i)),
			Method:    "GET",
			Path:      "/test",
			Timestamp: time.Now(),
		}
		inspector.addRecord(rec)
	}

	recent := inspector.GetRecent()

	// Records should be in reverse order (newest first)
	if recent[0].ID != "e" {
		t.Errorf("first record ID = %s, want 'e'", recent[0].ID)
	}
	if recent[4].ID != "a" {
		t.Errorf("last record ID = %s, want 'a'", recent[4].ID)
	}
}

func TestInspector_MaxSize(t *testing.T) {
	inspector := NewInspector()
	inspector.maxSize = 5 // Reduce for testing

	// Add more records than maxSize
	for i := 0; i < 10; i++ {
		rec := &RequestRecord{
			ID:        string(rune('a' + i)),
			Method:    "GET",
			Path:      "/test",
			Timestamp: time.Now(),
		}
		inspector.addRecord(rec)
	}

	recent := inspector.GetRecent()
	if len(recent) != 5 {
		t.Errorf("expected %d records, got %d", 5, len(recent))
	}

	// Should have the 5 most recent (j, i, h, g, f)
	if recent[0].ID != "j" {
		t.Errorf("first record ID = %s, want 'j'", recent[0].ID)
	}
}

func TestInspector_GetRequestByID(t *testing.T) {
	inspector := NewInspector()

	rec1 := &RequestRecord{ID: "id-1", Method: "GET", Path: "/one"}
	rec2 := &RequestRecord{ID: "id-2", Method: "POST", Path: "/two"}
	rec3 := &RequestRecord{ID: "id-3", Method: "PUT", Path: "/three"}

	inspector.addRecord(rec1)
	inspector.addRecord(rec2)
	inspector.addRecord(rec3)

	tests := []struct {
		name string
		id   string
		want *RequestRecord
	}{
		{name: "find first", id: "id-1", want: rec1},
		{name: "find second", id: "id-2", want: rec2},
		{name: "find third", id: "id-3", want: rec3},
		{name: "not found", id: "id-999", want: nil},
		{name: "empty id", id: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inspector.GetRequestByID(tt.id)
			if got == nil && tt.want == nil {
				return
			}
			if got == nil || tt.want == nil {
				t.Errorf("GetRequestByID(%s) = %v, want %v", tt.id, got, tt.want)
				return
			}
			if got.ID != tt.want.ID {
				t.Errorf("GetRequestByID(%s).ID = %s, want %s", tt.id, got.ID, tt.want.ID)
			}
		})
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{
			name: "valid websocket upgrade",
			headers: map[string]string{
				"Upgrade":    "websocket",
				"Connection": "Upgrade",
			},
			want: true,
		},
		{
			name: "websocket with keep-alive",
			headers: map[string]string{
				"Upgrade":    "websocket",
				"Connection": "Upgrade, keep-alive",
			},
			want: true,
		},
		{
			name: "case insensitive upgrade",
			headers: map[string]string{
				"Upgrade":    "WebSocket",
				"Connection": "upgrade",
			},
			want: true,
		},
		{
			name: "no upgrade header",
			headers: map[string]string{
				"Connection": "Upgrade",
			},
			want: false,
		},
		{
			name: "no connection header",
			headers: map[string]string{
				"Upgrade": "websocket",
			},
			want: false,
		},
		{
			name: "wrong upgrade value",
			headers: map[string]string{
				"Upgrade":    "h2c",
				"Connection": "Upgrade",
			},
			want: false,
		},
		{
			name: "connection without upgrade",
			headers: map[string]string{
				"Upgrade":    "websocket",
				"Connection": "keep-alive",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &http.Request{
				Header: make(http.Header),
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			got := isWebSocketUpgrade(req)
			if got != tt.want {
				t.Errorf("isWebSocketUpgrade() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldCloseConnection(t *testing.T) {
	tests := []struct {
		name       string
		reqProto   string
		reqHeaders map[string]string
		resHeaders map[string]string
		want       bool
	}{
		{
			name:     "HTTP/1.1 default keep-alive",
			reqProto: "HTTP/1.1",
			want:     false,
		},
		{
			name:       "HTTP/1.1 with Connection: close in request",
			reqProto:   "HTTP/1.1",
			reqHeaders: map[string]string{"Connection": "close"},
			want:       true,
		},
		{
			name:       "HTTP/1.1 with Connection: close in response",
			reqProto:   "HTTP/1.1",
			resHeaders: map[string]string{"Connection": "close"},
			want:       true,
		},
		{
			name:     "HTTP/1.0 default close",
			reqProto: "HTTP/1.0",
			want:     true,
		},
		{
			name:       "HTTP/1.0 with keep-alive",
			reqProto:   "HTTP/1.0",
			reqHeaders: map[string]string{"Connection": "keep-alive"},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &http.Request{
				Header:     make(http.Header),
				ProtoMajor: 1,
				ProtoMinor: 1,
			}
			if tt.reqProto == "HTTP/1.0" {
				req.ProtoMinor = 0
			}
			for k, v := range tt.reqHeaders {
				req.Header.Set(k, v)
			}

			res := &http.Response{
				Header: make(http.Header),
			}
			for k, v := range tt.resHeaders {
				res.Header.Set(k, v)
			}

			got := shouldCloseConnection(req, res)
			if got != tt.want {
				t.Errorf("shouldCloseConnection() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInspector_SetLocalAddr(t *testing.T) {
	inspector := NewInspector()

	if inspector.localAddr != "" {
		t.Errorf("initial localAddr = %q, want empty", inspector.localAddr)
	}

	inspector.SetLocalAddr("127.0.0.1:3000")

	if inspector.localAddr != "127.0.0.1:3000" {
		t.Errorf("localAddr = %q, want %q", inspector.localAddr, "127.0.0.1:3000")
	}
}

func TestInspector_SSEClientManagement(t *testing.T) {
	inspector := NewInspector()

	// Create a client channel
	ch := make(chan *RequestRecord, 10)

	// Add client
	inspector.addSSEClient(ch)

	inspector.sseClientsMu.RLock()
	_, exists := inspector.sseClients[ch]
	inspector.sseClientsMu.RUnlock()

	if !exists {
		t.Error("client should exist after adding")
	}

	// Remove client
	inspector.removeSSEClient(ch)

	inspector.sseClientsMu.RLock()
	_, exists = inspector.sseClients[ch]
	inspector.sseClientsMu.RUnlock()

	if exists {
		t.Error("client should not exist after removing")
	}
}
