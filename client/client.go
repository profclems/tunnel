package client

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/profclems/tunnel/protocol"
)

// TunnelConfig defines a single tunnel
type TunnelConfig struct {
	Type       string `json:"type"`                  // "http" or "tcp"
	Subdomain  string `json:"subdomain,omitempty"`   // for http
	RemotePort int    `json:"remote_port,omitempty"` // for tcp
	LocalAddr  string `json:"local_addr"`
	PublicURL  string `json:"public_url,omitempty"` // assigned by server

	// BasicAuth credentials in "user:password" format (optional, not serialized)
	BasicAuth string `json:"-"`
}

// Config holds client configuration
type Config struct {
	ServerAddr  string
	AuthToken   string
	Tunnels     []TunnelConfig
	Inspect     bool
	InspectPort int

	// Inspector template options
	Template     string // Template name: developer, minimal, terminal, modern, monitoring
	TemplatePath string // Custom template file path (overrides Template)

	// TLS options
	TLSEnabled      bool   // Enable TLS for server connection
	TLSInsecureSkip bool   // Skip server certificate verification
	TLSCertFile     string // Client certificate file (for mTLS)
	TLSKeyFile      string // Client key file (for mTLS)
	TLSCAFile       string // CA certificate for server verification
}

// Client manages the tunnel agent
type Client struct {
	config    Config
	logger    *slog.Logger
	inspector *Inspector

	// Thread-safe tunnel state
	tunnelsMu sync.RWMutex
	tunnels   []TunnelConfig

	// Control stream for dynamic tunnel operations
	ctlStreamMu sync.Mutex
	ctlStream   net.Conn

	// Pending requests for async responses
	pendingMu sync.Mutex
	pending   map[string]chan interface{}

	// Config manager for persistence
	configMgr *ConfigManager
}

// NewClient creates a new tunnel client
func NewClient(cfg Config, logger *slog.Logger) *Client {
	c := &Client{
		config:  cfg,
		logger:  logger,
		pending: make(map[string]chan interface{}),
	}
	if cfg.Inspect {
		var opts []InspectorOption
		if cfg.Template != "" {
			opts = append(opts, WithTemplate(cfg.Template))
		}
		if cfg.TemplatePath != "" {
			opts = append(opts, WithTemplatePath(cfg.TemplatePath))
		}
		c.inspector = NewInspector(opts...)
	}
	return c
}

// SetConfigManager sets the config manager for persistence
func (c *Client) SetConfigManager(cm *ConfigManager) {
	c.configMgr = cm
}

// GetInspector returns the inspector instance
func (c *Client) GetInspector() *Inspector {
	return c.inspector
}

// GetTunnels returns the current list of tunnels (thread-safe)
func (c *Client) GetTunnels() []TunnelConfig {
	c.tunnelsMu.RLock()
	defer c.tunnelsMu.RUnlock()

	tunnels := make([]TunnelConfig, len(c.tunnels))
	copy(tunnels, c.tunnels)
	return tunnels
}

// createTLSConfig creates a TLS configuration for connecting to the server
func (c *Client) createTLSConfig() (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.config.TLSInsecureSkip,
	}

	// Load client certificate if provided (for mTLS)
	if c.config.TLSCertFile != "" && c.config.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.config.TLSCertFile, c.config.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate for server verification
	if c.config.TLSCAFile != "" {
		caCert, err := os.ReadFile(c.config.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}

		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caPool
	}

	return tlsConfig, nil
}

// Start runs the client
func (c *Client) Start(ctx context.Context) error {
	if c.config.Inspect {
		// Wire up inspector with client and config manager for tunnel management
		c.inspector.SetClient(c)
		if c.configMgr != nil {
			c.inspector.SetConfigManager(c.configMgr)
		}
		go c.inspector.ServeDashboard(ctx, c.config.InspectPort)
	}

	// Simple retry loop for connection
	backoff := time.Second

	for {
		if err := c.connectAndServe(ctx); err != nil {
			c.logger.Error("connection failed", "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			// Exponential backoff with cap
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	c.logger.Info("connecting to server", "addr", c.config.ServerAddr)

	var conn net.Conn
	var err error

	if c.config.TLSEnabled {
		tlsConfig, tlsErr := c.createTLSConfig()
		if tlsErr != nil {
			return fmt.Errorf("failed to create TLS config: %w", tlsErr)
		}
		dialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 10 * time.Second},
			Config:    tlsConfig,
		}
		conn, err = dialer.DialContext(ctx, "tcp", c.config.ServerAddr)
		if err != nil {
			return fmt.Errorf("TLS dial failed: %w", err)
		}
		c.logger.Info("connected with TLS", "mtls", c.config.TLSCertFile != "")
	} else {
		conn, err = net.DialTimeout("tcp", c.config.ServerAddr, 10*time.Second)
		if err != nil {
			return err
		}
	}
	defer conn.Close()

	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 30 * time.Second
	cfg.ConnectionWriteTimeout = 60 * time.Second // Increase from 10s default
	cfg.MaxStreamWindowSize = 1024 * 1024         // 1MB window (from 256KB)
	session, err := yamux.Client(conn, cfg)
	if err != nil {
		return fmt.Errorf("yamux setup failed: %w", err)
	}

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ctlStream, err := session.Open()
	if err != nil {
		return fmt.Errorf("failed to open control stream: %w", err)
	}
	var tunnels []protocol.TunnelRequest
	for _, t := range c.config.Tunnels {
		tunnels = append(tunnels, protocol.TunnelRequest{
			Type:       t.Type,
			Subdomain:  t.Subdomain,
			RemotePort: t.RemotePort,
		})
	}

	req := protocol.AuthRequest{
		Token:   c.config.AuthToken,
		Tunnels: tunnels,
	}
	if err := protocol.WriteMessage(ctlStream, protocol.MsgAuthRequest, req); err != nil {
		return fmt.Errorf("handshake write failed: %w", err)
	}

	respMsg, err := protocol.ReadMessage(ctlStream)
	if err != nil {
		return fmt.Errorf("handshake read failed: %w", err)
	}

	if respMsg.Type != protocol.MsgAuthResponse {
		return fmt.Errorf("unexpected message type: %s", respMsg.Type)
	}

	var resp protocol.AuthResponse
	if err := json.Unmarshal(respMsg.Payload, &resp); err != nil {
		return fmt.Errorf("invalid auth response: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("server rejected auth: %s", resp.Error)
	}

	// Update tunnel configs with assigned subdomains from server
	// Server returns URLs in the same order as tunnel requests
	c.tunnelsMu.Lock()
	c.tunnels = make([]TunnelConfig, len(c.config.Tunnels))
	copy(c.tunnels, c.config.Tunnels)

	for i, tunnelURL := range resp.URLs {
		c.logger.Info("tunnel established", "url", tunnelURL)

		if i < len(c.tunnels) {
			// Set the public URL
			c.tunnels[i].PublicURL = tunnelURL

			// For HTTP tunnels without explicit subdomain, extract the assigned one
			if c.tunnels[i].Type == "http" && c.tunnels[i].Subdomain == "" {
				if subdomain := extractSubdomainFromURL(tunnelURL); subdomain != "" {
					c.tunnels[i].Subdomain = subdomain
					c.config.Tunnels[i].Subdomain = subdomain
				}
			}
		}
	}
	c.tunnelsMu.Unlock()

	// Store control stream for dynamic tunnel operations
	c.ctlStreamMu.Lock()
	c.ctlStream = ctlStream
	c.ctlStreamMu.Unlock()

	// Start control response handler
	go c.handleControlResponses(ctlStream)

	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-session.CloseChan():
		}
		cancel()
	}()

	for {
		stream, err := session.Accept()
		if err != nil {
			// Check if we are shutting down
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			if err == io.EOF || errors.Is(err, yamux.ErrSessionShutdown) {
				return nil // Clean shutdown
			}
			return fmt.Errorf("accept stream failed: %w", err)
		}

		go c.handleStream(serveCtx, stream)
	}
}

func (c *Client) handleStream(ctx context.Context, remote net.Conn) {
	msg, bufferedReader, err := protocol.ReadMessageBuffered(remote)
	if err != nil {
		c.logger.Error("failed to read tunnel init", "error", err)
		remote.Close()
		return
	}

	if msg.Type != protocol.MsgTunnelInit {
		c.logger.Error("unexpected message type on data stream", "type", msg.Type)
		remote.Close()
		return
	}

	var init protocol.TunnelInit
	if err := json.Unmarshal(msg.Payload, &init); err != nil {
		c.logger.Error("invalid tunnel init payload", "error", err)
		remote.Close()
		return
	}

	// Thread-safe tunnel lookup
	c.tunnelsMu.RLock()
	var target TunnelConfig
	found := false
	for _, t := range c.tunnels {
		if t.Type == "http" && t.Subdomain == init.Subdomain {
			target = t
			found = true
			break
		}
		if t.Type == "tcp" && init.Subdomain == fmt.Sprintf("tcp:%d", t.RemotePort) {
			target = t
			found = true
			break
		}
	}
	c.tunnelsMu.RUnlock()

	if !found {
		c.logger.Error("unknown subdomain requested", "subdomain", init.Subdomain)
		remote.Close()
		return
	}

	// Handle based on tunnel type
	if target.Type == "http" {
		if c.config.Inspect {
			c.handleInspectedStream(bufferedReader, remote, target)
		} else {
			c.handleHTTPStream(bufferedReader, remote, target)
		}
	} else {
		c.handleTCPStream(bufferedReader, remote, target.LocalAddr)
	}
}

// handleHTTPStream handles HTTP tunnel traffic using proper HTTP client/response lifecycle.
// This avoids the io.Copy goroutine race conditions that caused file truncation.
func (c *Client) handleHTTPStream(bufferedReader io.Reader, remote net.Conn, target TunnelConfig) {
	defer remote.Close()

	// Read the incoming HTTP request from the server
	req, err := http.ReadRequest(bufio.NewReader(bufferedReader))
	if err != nil {
		c.logger.Error("failed to read HTTP request", "error", err)
		c.writeErrorResponse(remote, http.StatusBadGateway, "Failed to read request")
		return
	}

	// Check basic auth if configured
	if !c.checkBasicAuth(req, remote, target.BasicAuth) {
		return // 401 already sent
	}

	// Strip auth header before forwarding (don't leak tunnel auth to local service)
	req.Header.Del("Authorization")

	// Modify request for forwarding to local service
	req.URL.Scheme = "http"
	req.URL.Host = target.LocalAddr
	req.RequestURI = "" // Required for http.Client

	// Create HTTP client for forwarding
	// Use a transport with proper connection handling
	transport := &http.Transport{
		DisableCompression: true, // Preserve original encoding
		// Don't limit idle connections since each request uses a new stream
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // No timeout - let yamux handle it
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects, pass them through
		},
	}

	// Forward request to local service
	resp, err := client.Do(req)
	if err != nil {
		c.logger.Error("failed to forward request to local service", "error", err, "addr", target.LocalAddr)
		c.writeErrorResponse(remote, http.StatusBadGateway, "Local service unavailable")
		return
	}
	defer resp.Body.Close()

	// Write response back to the yamux stream
	// This properly handles Content-Length, chunked encoding, and body streaming
	if err := resp.Write(remote); err != nil {
		c.logger.Error("failed to write response to tunnel", "error", err)
	}
}

// writeErrorResponse writes an HTTP error response to the stream
func (c *Client) writeErrorResponse(w io.Writer, statusCode int, message string) {
	resp := &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(message)),
	}
	resp.Header.Set("Content-Type", "text/plain")
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(message)))
	resp.Write(w)
}

// checkBasicAuth validates the Authorization header against configured credentials.
// Returns true if auth is valid or not configured.
// Returns false and writes 401 response if auth is required but invalid.
func (c *Client) checkBasicAuth(req *http.Request, conn net.Conn, authConfig string) bool {
	if authConfig == "" {
		return true // No auth configured
	}

	auth := req.Header.Get("Authorization")
	if auth == "" {
		c.writeAuthRequired(conn, "")
		return false
	}

	// Parse "Basic <base64>"
	const prefix = "Basic "
	if !strings.HasPrefix(auth, prefix) {
		c.writeAuthRequired(conn, "")
		return false
	}

	decoded, err := base64.StdEncoding.DecodeString(auth[len(prefix):])
	if err != nil {
		c.writeAuthRequired(conn, "")
		return false
	}

	// Compare credentials using constant-time comparison to prevent timing attacks
	if subtle.ConstantTimeCompare([]byte(authConfig), decoded) != 1 {
		c.writeAuthRequired(conn, "")
		return false
	}

	return true
}

// writeAuthRequired writes a 401 Unauthorized response with WWW-Authenticate header
func (c *Client) writeAuthRequired(conn net.Conn, realm string) {
	if realm == "" {
		realm = "tunnel"
	}
	body := "Unauthorized"
	resp := &http.Response{
		StatusCode:    http.StatusUnauthorized,
		Status:        http.StatusText(http.StatusUnauthorized),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	resp.Header.Set("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, realm))
	resp.Header.Set("Content-Type", "text/plain")
	resp.Write(conn)
}

// handleTCPStream handles raw TCP tunnel traffic using bidirectional copy.
// TCP tunnels don't have HTTP semantics so manual io.Copy is appropriate.
func (c *Client) handleTCPStream(bufferedReader io.Reader, remote net.Conn, localAddr string) {
	defer remote.Close()

	local, err := net.Dial("tcp", localAddr)
	if err != nil {
		c.logger.Error("failed to dial local service", "error", err)
		return
	}
	defer local.Close()

	// Disable Nagle's algorithm for low-latency interactive sessions (SSH, etc.)
	if tc, ok := local.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(local, bufferedReader)
		if tc, ok := local.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		io.Copy(remote, local)
	}()

	wg.Wait()
}

func (c *Client) handleInspectedStream(bufferedReader io.Reader, remote net.Conn, target TunnelConfig) {
	defer remote.Close()

	c.inspector.SetLocalAddr(target.LocalAddr)

	// Read the incoming HTTP request
	req, err := http.ReadRequest(bufio.NewReader(bufferedReader))
	if err != nil {
		c.logger.Error("failed to read HTTP request for inspection", "error", err)
		c.writeErrorResponse(remote, http.StatusBadGateway, "Failed to read request")
		return
	}

	// Use Inspector to handle request/response with recording
	// Pass auth config so inspector can record auth status
	c.inspector.InspectHTTPWithOptions(req, remote, InspectHTTPOptions{
		LocalAddr: target.LocalAddr,
		BasicAuth: target.BasicAuth,
	})
}

// extractSubdomainFromURL extracts the subdomain from a tunnel URL.
// For "https://abc123.example.com" returns "abc123".
func extractSubdomainFromURL(rawURL string) string {
	// Remove leading scheme if missing for url.Parse
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	hostname := parsed.Hostname()
	if idx := strings.Index(hostname, "."); idx != -1 {
		return hostname[:idx]
	}
	return ""
}

// handleControlResponses handles async responses from the server for dynamic tunnel operations
func (c *Client) handleControlResponses(ctlStream net.Conn) {
	c.logger.Info("control response handler started")

	// Use ReadMessageBuffered once to get the buffered reader, then reuse it
	// This prevents data loss from creating new bufio.Readers on each iteration
	var reader io.Reader = ctlStream

	for {
		msg, nextReader, err := protocol.ReadMessageBuffered(reader)
		if err != nil {
			c.logger.Info("control response handler exiting", "error", err)
			return // Stream closed
		}
		reader = nextReader // Reuse the buffered reader

		c.logger.Info("received control response", "type", msg.Type)

		switch msg.Type {
		case protocol.MsgTunnelAddResponse:
			var resp protocol.TunnelAddResponse
			if err := json.Unmarshal(msg.Payload, &resp); err == nil {
				c.logger.Info("dispatching tunnel add response", "requestID", resp.RequestID, "success", resp.Success)
				c.handlePendingResponse(resp.RequestID, resp)
			}

		case protocol.MsgTunnelRemoveResponse:
			var resp protocol.TunnelRemoveResponse
			if err := json.Unmarshal(msg.Payload, &resp); err == nil {
				c.logger.Info("dispatching tunnel remove response", "requestID", resp.RequestID, "success", resp.Success)
				c.handlePendingResponse(resp.RequestID, resp)
			}
		}
	}
}

func (c *Client) handlePendingResponse(requestID string, resp interface{}) {
	c.pendingMu.Lock()
	ch, ok := c.pending[requestID]
	if ok {
		delete(c.pending, requestID)
	}
	c.pendingMu.Unlock()

	if ok {
		ch <- resp
		close(ch)
	}
}

// AddTunnel dynamically adds a new tunnel
func (c *Client) AddTunnel(t TunnelConfig) (*protocol.TunnelAddResponse, error) {
	requestID := uuid.NewString()
	c.logger.Info("adding tunnel", "requestID", requestID, "type", t.Type, "subdomain", t.Subdomain, "localAddr", t.LocalAddr)

	req := protocol.TunnelAddRequest{
		RequestID: requestID,
		Tunnel: protocol.TunnelRequest{
			Type:       t.Type,
			Subdomain:  t.Subdomain,
			RemotePort: t.RemotePort,
		},
		LocalAddr: t.LocalAddr,
	}

	// Create response channel
	respCh := make(chan interface{}, 1)
	c.pendingMu.Lock()
	c.pending[requestID] = respCh
	c.pendingMu.Unlock()

	// Send request
	c.ctlStreamMu.Lock()
	if c.ctlStream == nil {
		c.ctlStreamMu.Unlock()
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()
		c.logger.Error("not connected to server")
		return nil, fmt.Errorf("not connected to server")
	}
	c.logger.Info("sending tunnel add request to server", "requestID", requestID)
	err := protocol.WriteMessage(c.ctlStream, protocol.MsgTunnelAdd, req)
	c.ctlStreamMu.Unlock()

	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()
		c.logger.Error("failed to send tunnel add request", "error", err)
		return nil, fmt.Errorf("failed to send tunnel add request: %w", err)
	}

	c.logger.Info("tunnel add request sent, waiting for response", "requestID", requestID)

	// Wait for response with timeout
	select {
	case resp := <-respCh:
		addResp := resp.(protocol.TunnelAddResponse)
		if addResp.Success {
			// Update local tunnel list
			c.tunnelsMu.Lock()
			// Update subdomain/port if it was auto-assigned
			if t.Type == "http" && t.Subdomain == "" && addResp.URL != "" {
				t.Subdomain = extractSubdomainFromURL(addResp.URL)
			}
			if t.Type == "tcp" && addResp.AssignedPort != 0 {
				t.RemotePort = addResp.AssignedPort
			}
			// Set public URL from server response
			t.PublicURL = addResp.URL
			c.tunnels = append(c.tunnels, t)
			c.tunnelsMu.Unlock()
		}
		return &addResp, nil

	case <-time.After(30 * time.Second):
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("timeout waiting for tunnel add response")
	}
}

// RemoveTunnel dynamically removes a tunnel
func (c *Client) RemoveTunnel(tunnelType, subdomain string, port int) (*protocol.TunnelRemoveResponse, error) {
	requestID := uuid.NewString()

	req := protocol.TunnelRemoveRequest{
		RequestID:  requestID,
		Type:       tunnelType,
		Subdomain:  subdomain,
		RemotePort: port,
	}

	// Create response channel
	respCh := make(chan interface{}, 1)
	c.pendingMu.Lock()
	c.pending[requestID] = respCh
	c.pendingMu.Unlock()

	// Send request
	c.ctlStreamMu.Lock()
	if c.ctlStream == nil {
		c.ctlStreamMu.Unlock()
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("not connected to server")
	}
	err := protocol.WriteMessage(c.ctlStream, protocol.MsgTunnelRemove, req)
	c.ctlStreamMu.Unlock()

	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("failed to send tunnel remove request: %w", err)
	}

	// Wait for response with timeout
	select {
	case resp := <-respCh:
		removeResp := resp.(protocol.TunnelRemoveResponse)
		if removeResp.Success {
			// Update local tunnel list
			c.tunnelsMu.Lock()
			for i, t := range c.tunnels {
				if t.Type == tunnelType {
					if tunnelType == "http" && t.Subdomain == subdomain {
						c.tunnels = append(c.tunnels[:i], c.tunnels[i+1:]...)
						break
					}
					if tunnelType == "tcp" && t.RemotePort == port {
						c.tunnels = append(c.tunnels[:i], c.tunnels[i+1:]...)
						break
					}
				}
			}
			c.tunnelsMu.Unlock()
		}
		return &removeResp, nil

	case <-time.After(30 * time.Second):
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("timeout waiting for tunnel remove response")
	}
}
