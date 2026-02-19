package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/profclems/tunnel/protocol"
	"github.com/hashicorp/yamux"
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
	cfg.ConnectionWriteTimeout = 60 * time.Second  // Increase from 10s default
	cfg.MaxStreamWindowSize = 1024 * 1024          // 1MB window (from 256KB)
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

	for _, url := range resp.URLs {
		c.logger.Info("tunnel established", "url", url)
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

	if c.config.Inspect && target.Type == "http" {
		c.handleInspectedStream(bufferedReader, remote, target.LocalAddr)
		return
	}

	defer remote.Close()

	local, err := net.Dial("tcp", target.LocalAddr)
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
		if c, ok := local.(*net.TCPConn); ok {
			c.CloseWrite()
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

	local, err := net.Dial("tcp", localAddr)
	if err != nil {
		c.logger.Error("failed to dial local service", "error", err)
		return
	}
	defer local.Close()

	// Use Inspector to proxy
	c.inspector.Inspect(bufferedReader, remote, local)
}
