package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
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

	"github.com/hashicorp/yamux"
	"github.com/profclems/tunnel/protocol"
)

// TunnelConfig defines a single tunnel
type TunnelConfig struct {
	Type       string // "http" or "tcp"
	Subdomain  string // for http
	RemotePort int    // for tcp
	LocalAddr  string
}

// Config holds client configuration
type Config struct {
	ServerAddr  string
	AuthToken   string
	Tunnels     []TunnelConfig
	Inspect     bool
	InspectPort int

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
}

// NewClient creates a new tunnel client
func NewClient(cfg Config, logger *slog.Logger) *Client {
	c := &Client{
		config: cfg,
		logger: logger,
	}
	if cfg.Inspect {
		c.inspector = NewInspector()
	}
	return c
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
	for i, url := range resp.URLs {
		c.logger.Info("tunnel established", "url", url)

		// For HTTP tunnels without explicit subdomain, extract the assigned one
		if i < len(c.config.Tunnels) && c.config.Tunnels[i].Type == "http" && c.config.Tunnels[i].Subdomain == "" {
			if subdomain := extractSubdomainFromURL(url); subdomain != "" {
				c.config.Tunnels[i].Subdomain = subdomain
			}
		}
	}

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

	var target TunnelConfig
	found := false
	for _, t := range c.config.Tunnels {
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

	if !found {
		c.logger.Error("unknown subdomain requested", "subdomain", init.Subdomain)
		remote.Close()
		return
	}

	// Handle based on tunnel type
	if target.Type == "http" {
		if c.config.Inspect {
			c.handleInspectedStream(bufferedReader, remote, target.LocalAddr)
		} else {
			c.handleHTTPStream(bufferedReader, remote, target.LocalAddr)
		}
	} else {
		c.handleTCPStream(bufferedReader, remote, target.LocalAddr)
	}
}

// handleHTTPStream handles HTTP tunnel traffic using proper HTTP client/response lifecycle.
// This avoids the io.Copy goroutine race conditions that caused file truncation.
func (c *Client) handleHTTPStream(bufferedReader io.Reader, remote net.Conn, localAddr string) {
	defer remote.Close()

	// Read the incoming HTTP request from the server
	req, err := http.ReadRequest(bufio.NewReader(bufferedReader))
	if err != nil {
		c.logger.Error("failed to read HTTP request", "error", err)
		c.writeErrorResponse(remote, http.StatusBadGateway, "Failed to read request")
		return
	}

	// Modify request for forwarding to local service
	req.URL.Scheme = "http"
	req.URL.Host = localAddr
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
		c.logger.Error("failed to forward request to local service", "error", err, "addr", localAddr)
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

func (c *Client) handleInspectedStream(bufferedReader io.Reader, remote net.Conn, localAddr string) {
	defer remote.Close()

	c.inspector.SetLocalAddr(localAddr)

	// Read the incoming HTTP request
	req, err := http.ReadRequest(bufio.NewReader(bufferedReader))
	if err != nil {
		c.logger.Error("failed to read HTTP request for inspection", "error", err)
		c.writeErrorResponse(remote, http.StatusBadGateway, "Failed to read request")
		return
	}

	// Use Inspector to handle request/response with recording
	c.inspector.InspectHTTP(req, remote, localAddr)
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
