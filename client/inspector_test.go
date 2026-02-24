package client

import (
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
	for i := range 5 {
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
	for i := range 10 {
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
