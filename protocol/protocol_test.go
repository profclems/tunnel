package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteMessage(t *testing.T) {
	tests := []struct {
		name    string
		msgType MessageType
		payload any
	}{
		{
			name:    "auth request with http tunnel",
			msgType: MsgAuthRequest,
			payload: AuthRequest{
				Token: "secret",
				Tunnels: []TunnelRequest{
					{Type: "http", Subdomain: "myapp"},
				},
			},
		},
		{
			name:    "auth request with tcp tunnel",
			msgType: MsgAuthRequest,
			payload: AuthRequest{
				Token: "secret",
				Tunnels: []TunnelRequest{
					{Type: "tcp", RemotePort: 2222},
				},
			},
		},
		{
			name:    "auth request with multiple tunnels",
			msgType: MsgAuthRequest,
			payload: AuthRequest{
				Token: "secret",
				Tunnels: []TunnelRequest{
					{Type: "http", Subdomain: "web"},
					{Type: "tcp", RemotePort: 22},
				},
			},
		},
		{
			name:    "auth response success",
			msgType: MsgAuthResponse,
			payload: AuthResponse{Success: true, URLs: []string{"https://myapp.example.com"}},
		},
		{
			name:    "auth response error",
			msgType: MsgAuthResponse,
			payload: AuthResponse{Success: false, Error: "invalid token"},
		},
		{
			name:    "tunnel init",
			msgType: MsgTunnelInit,
			payload: TunnelInit{Subdomain: "myapp"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := WriteMessage(&buf, tt.msgType, tt.payload)
			if err != nil {
				t.Fatalf("WriteMessage() error = %v", err)
			}

			// Verify it's valid JSON
			var msg ControlMessage
			if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
				t.Fatalf("WriteMessage() produced invalid JSON: %v", err)
			}

			if msg.Type != tt.msgType {
				t.Errorf("WriteMessage() type = %v, want %v", msg.Type, tt.msgType)
			}
		})
	}
}

func TestReadMessage(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType MessageType
		wantErr  bool
	}{
		{
			name:     "valid auth request",
			input:    "{\"type\":\"AUTH_REQ\",\"payload\":{\"token\":\"secret\",\"tunnels\":[{\"type\":\"http\",\"subdomain\":\"myapp\"}]}}\n",
			wantType: MsgAuthRequest,
			wantErr:  false,
		},
		{
			name:     "valid auth response",
			input:    "{\"type\":\"AUTH_RES\",\"payload\":{\"success\":true,\"urls\":[\"https://myapp.example.com\"]}}\n",
			wantType: MsgAuthResponse,
			wantErr:  false,
		},
		{
			name:     "valid tunnel init",
			input:    "{\"type\":\"TUNNEL_INIT\",\"payload\":{\"subdomain\":\"myapp\"}}\n",
			wantType: MsgTunnelInit,
			wantErr:  false,
		},
		{
			name:    "invalid json",
			input:   `{invalid`,
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := bytes.NewBufferString(tt.input)
			msg, err := ReadMessage(buf)

			if tt.wantErr {
				if err == nil {
					t.Error("ReadMessage() expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("ReadMessage() unexpected error: %v", err)
			}

			if msg.Type != tt.wantType {
				t.Errorf("ReadMessage() type = %v, want %v", msg.Type, tt.wantType)
			}
		})
	}
}

func TestRoundTrip_AuthRequest(t *testing.T) {
	original := AuthRequest{
		Token: "my-secret-token",
		Tunnels: []TunnelRequest{
			{Type: "http", Subdomain: "myapp"},
			{Type: "tcp", RemotePort: 22},
		},
	}

	// Write
	var buf bytes.Buffer
	if err := WriteMessage(&buf, MsgAuthRequest, original); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}

	// Read
	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}

	if msg.Type != MsgAuthRequest {
		t.Errorf("Round trip type = %v, want %v", msg.Type, MsgAuthRequest)
	}

	// Unmarshal payload
	var decoded AuthRequest
	if err := json.Unmarshal(msg.Payload, &decoded); err != nil {
		t.Fatalf("Unmarshal payload error = %v", err)
	}

	if decoded.Token != original.Token {
		t.Errorf("Round trip token = %v, want %v", decoded.Token, original.Token)
	}
	if len(decoded.Tunnels) != len(original.Tunnels) {
		t.Errorf("Round trip tunnels count = %v, want %v", len(decoded.Tunnels), len(original.Tunnels))
	}
	if decoded.Tunnels[0].Type != original.Tunnels[0].Type {
		t.Errorf("Round trip tunnel[0].Type = %v, want %v", decoded.Tunnels[0].Type, original.Tunnels[0].Type)
	}
	if decoded.Tunnels[0].Subdomain != original.Tunnels[0].Subdomain {
		t.Errorf("Round trip tunnel[0].Subdomain = %v, want %v", decoded.Tunnels[0].Subdomain, original.Tunnels[0].Subdomain)
	}
	if decoded.Tunnels[1].RemotePort != original.Tunnels[1].RemotePort {
		t.Errorf("Round trip tunnel[1].RemotePort = %v, want %v", decoded.Tunnels[1].RemotePort, original.Tunnels[1].RemotePort)
	}
}

func TestRoundTrip_TunnelInit(t *testing.T) {
	original := TunnelInit{Subdomain: "myapp"}

	var buf bytes.Buffer
	if err := WriteMessage(&buf, MsgTunnelInit, original); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}

	msg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}

	if msg.Type != MsgTunnelInit {
		t.Errorf("Round trip type = %v, want %v", msg.Type, MsgTunnelInit)
	}

	var decoded TunnelInit
	if err := json.Unmarshal(msg.Payload, &decoded); err != nil {
		t.Fatalf("Unmarshal payload error = %v", err)
	}

	if decoded.Subdomain != original.Subdomain {
		t.Errorf("Round trip subdomain = %v, want %v", decoded.Subdomain, original.Subdomain)
	}
}
