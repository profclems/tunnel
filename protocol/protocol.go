package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// MessageType defines the type of control message
type MessageType string

const (
	MsgAuthRequest  MessageType = "AUTH_REQ"
	MsgAuthResponse MessageType = "AUTH_RES"
	MsgTunnelInit   MessageType = "TUNNEL_INIT"

	// Dynamic tunnel management
	MsgTunnelAdd            MessageType = "TUNNEL_ADD"
	MsgTunnelAddResponse    MessageType = "TUNNEL_ADD_RES"
	MsgTunnelRemove         MessageType = "TUNNEL_REMOVE"
	MsgTunnelRemoveResponse MessageType = "TUNNEL_REMOVE_RES"
)

// ControlMessage is the envelope for control plane communication
type ControlMessage struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// AuthRequest is sent by the client to register
type AuthRequest struct {
	Token   string          `json:"token"`
	Tunnels []TunnelRequest `json:"tunnels"`
}

type TunnelRequest struct {
	Type       string `json:"type"` // "http" or "tcp"
	Subdomain  string `json:"subdomain,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
}

// AuthResponse is sent by the server to confirm registration
type AuthResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	// Map of request index/id to public URL
	URLs []string `json:"urls,omitempty"`
}

// TunnelInit is sent as the first message on a new data stream
type TunnelInit struct {
	Subdomain string `json:"subdomain"`
}

// TunnelAddRequest is sent by the client to dynamically add a new tunnel
type TunnelAddRequest struct {
	RequestID string        `json:"request_id"`
	Tunnel    TunnelRequest `json:"tunnel"`
	LocalAddr string        `json:"local_addr"`
}

// TunnelAddResponse is sent by the server confirming tunnel addition
type TunnelAddResponse struct {
	RequestID    string `json:"request_id"`
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
	URL          string `json:"url,omitempty"`
	AssignedPort int    `json:"assigned_port,omitempty"`
}

// TunnelRemoveRequest is sent by the client to remove an existing tunnel
type TunnelRemoveRequest struct {
	RequestID  string `json:"request_id"`
	Type       string `json:"type"`
	Subdomain  string `json:"subdomain,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
}

// TunnelRemoveResponse is sent by the server confirming tunnel removal
type TunnelRemoveResponse struct {
	RequestID string `json:"request_id"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

// WriteMessage sends a JSON control message
func WriteMessage(w io.Writer, msgType MessageType, payload any) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	wrapper := ControlMessage{
		Type:    msgType,
		Payload: payloadBytes,
	}

	return json.NewEncoder(w).Encode(wrapper)
}

// ReadMessage reads a JSON control message from a reader.
// DEPRECATED: Use ReadMessageBuffered for streams that have data after the message.
func ReadMessage(r io.Reader) (*ControlMessage, error) {
	msg, _, err := ReadMessageBuffered(r)
	return msg, err
}

// ReadMessageBuffered reads a JSON control message and returns a reader for remaining data.
// This is important when the stream contains additional data after the message,
// since buffering may have read ahead. Always use the returned reader for subsequent reads.
func ReadMessageBuffered(r io.Reader) (*ControlMessage, io.Reader, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}

	// ReadBytes reads until newline (inclusive)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, nil, err
	}

	var msg ControlMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, nil, err
	}

	return &msg, br, nil
}
