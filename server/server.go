package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/profclems/tunnel/protocol"
	"golang.org/x/crypto/acme/autocert"
)

// Config holds server configuration
type Config struct {
	ControlPort int
	PublicPort  int // Usually 80
	TLSPort     int // Usually 443
	Domain      string
	AuthToken   string
	TLSEmail    string // For Let's Encrypt
	HealthPort  int    // Health check endpoint port (0 to disable)
	MetricsPort int    // Metrics endpoint port (0 to disable)
	RateLimit   int    // Requests per second per subdomain (0 to disable)
	RateBurst   int    // Burst capacity for rate limiting

	// mTLS configuration for agent connections
	AgentCAFile      string // CA certificate for verifying agent client certs
	AgentCertFile    string // Server certificate for agent connections
	AgentKeyFile     string // Server key for agent connections
	RequireAgentCert bool   // Require client certificates from agents
}

// AgentSession represents a connected tunnel agent
type AgentSession struct {
	ID string
	// Subdomains owned by this session
	Subdomains []string
	// TCP Ports owned by this session
	TCPPorts    []int
	Session     *yamux.Session
	Conn        net.Conn
	ConnectedAt time.Time
}

// Server manages the tunnel server
type Server struct {
	config Config
	logger *slog.Logger

	// Map Subdomain -> Session
	httpRegistry map[string]*AgentSession

	// Map Port -> Session
	tcpRegistry map[int]*AgentSession

	mu sync.RWMutex

	controlListener net.Listener
	tcpListeners    map[int]net.Listener // Active TCP listeners

	startedAt   time.Time    // Server start time for uptime calculation
	rateLimiter *RateLimiter // Rate limiter for HTTP requests (nil if disabled)
	metrics     *Metrics     // Metrics collector (nil if disabled)
}

// NewServer creates a new tunnel server
func NewServer(cfg Config, logger *slog.Logger) *Server {
	s := &Server{
		config:       cfg,
		logger:       logger,
		httpRegistry: make(map[string]*AgentSession),
		tcpRegistry:  make(map[int]*AgentSession),
		tcpListeners: make(map[int]net.Listener),
		startedAt:    time.Now(),
	}

	// Initialize rate limiter if enabled
	if cfg.RateLimit > 0 {
		burst := cfg.RateBurst
		if burst == 0 {
			burst = cfg.RateLimit * 2 // Default burst to 2x the rate
		}
		s.rateLimiter = NewRateLimiter(float64(cfg.RateLimit), burst)
		// Start background cleanup to prevent memory leaks from stale buckets
		s.rateLimiter.StartCleanup(5*time.Minute, 10*time.Minute, make(chan struct{}))
	}

	// Initialize metrics if enabled
	if cfg.MetricsPort > 0 {
		s.metrics = NewMetrics()
	}

	return s
}

// Start runs the server (blocking)
func (s *Server) Start(ctx context.Context) error {
	// Start Control Plane
	go s.listenControl(ctx)

	// Start Public HTTP (Redirect to HTTPS if TLS enabled, or serve directly)
	go s.listenHTTP(ctx)

	// Start Health Check endpoint if configured
	if s.config.HealthPort > 0 {
		go s.listenHealth(ctx)
	}

	// Start Metrics endpoint if configured
	if s.config.MetricsPort > 0 && s.metrics != nil {
		go s.metrics.ServeMetrics(ctx, s.config.MetricsPort, s.logger)
	}

	// Start Public HTTPS
	if s.config.TLSEmail != "" {
		return s.listenHTTPS(ctx)
	}

	// Block forever if no TLS (since listenHTTP is in goroutine)
	<-ctx.Done()
	return nil
}

func (s *Server) listenControl(ctx context.Context) {
	addr := fmt.Sprintf(":%d", s.config.ControlPort)

	var l net.Listener
	var err error

	// Use TLS if agent certificates are configured
	if s.config.AgentCertFile != "" && s.config.AgentKeyFile != "" {
		tlsCfg, tlsErr := CreateAgentTLSConfig(MTLSConfig{
			AgentCAFile:       s.config.AgentCAFile,
			AgentCertFile:     s.config.AgentCertFile,
			AgentKeyFile:      s.config.AgentKeyFile,
			RequireClientCert: s.config.RequireAgentCert,
		})
		if tlsErr != nil {
			s.logger.Error("failed to create TLS config", "error", tlsErr)
			return
		}
		l, err = tls.Listen("tcp", addr, tlsCfg)
		if err == nil {
			s.logger.Info("control plane listening with TLS", "addr", addr, "mtls", s.config.AgentCAFile != "")
		}
	} else {
		l, err = net.Listen("tcp", addr)
		if err == nil {
			s.logger.Info("control plane listening", "addr", addr)
		}
	}

	if err != nil {
		s.logger.Error("failed to listen on control port", "addr", addr, "error", err)
		return
	}
	s.controlListener = l

	// Graceful shutdown routine
	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				s.logger.Error("accept error", "error", err)
				continue
			}
		}
		go s.handleAgent(ctx, conn)
	}
}

func (s *Server) listenHTTPS(ctx context.Context) error {
	// Custom host policy that allows the base domain and all subdomains
	hostPolicy := func(ctx context.Context, host string) error {
		// Allow exact match
		if host == s.config.Domain {
			return nil
		}
		// Allow subdomains (*.domain)
		if strings.HasSuffix(host, "."+s.config.Domain) {
			return nil
		}
		return fmt.Errorf("host %q not allowed", host)
	}

	m := &autocert.Manager{
		Cache:      autocert.DirCache("certs"),
		Prompt:     autocert.AcceptTOS,
		Email:      s.config.TLSEmail,
		HostPolicy: hostPolicy,
	}

	addr := fmt.Sprintf(":%d", s.config.TLSPort)
	s.logger.Info("public https listening", "addr", addr)

	// Use http.Server to handle HTTP/1.1 and HTTP/2 properly
	server := &http.Server{
		Addr:      addr,
		Handler:   s.httpHandler("https"),
		TLSConfig: m.TLSConfig(),
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	err := server.ListenAndServeTLS("", "")
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) listenHTTP(ctx context.Context) {
	addr := fmt.Sprintf(":%d", s.config.PublicPort)
	s.logger.Info("public http listening", "addr", addr)

	server := &http.Server{
		Addr:    addr,
		Handler: s.httpHandler("http"),
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.logger.Error("public http listener failed", "error", err)
	}
}

// httpHandler returns an http.Handler that forwards requests to tunnel agents
func (s *Server) httpHandler(proto string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		// Remove port if present
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		subdomain := strings.Split(host, ".")[0]

		// Rate limiting
		if s.rateLimiter != nil && !s.rateLimiter.Allow(subdomain) {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		// Find the agent for this subdomain
		s.mu.RLock()
		agent, ok := s.httpRegistry[subdomain]
		s.mu.RUnlock()

		if !ok {
			http.Error(w, "Tunnel not found", http.StatusNotFound)
			return
		}

		if s.metrics != nil {
			s.metrics.RecordRequest(subdomain)
			s.metrics.RecordConnection(subdomain)
		}

		// Get client IP for proxy headers
		clientIP := extractClientIP(r.RemoteAddr)

		// Create a reverse proxy with a custom transport that uses yamux streams
		target := &url.URL{
			Scheme: "http",
			Host:   "tunnel-agent", // Placeholder, we use custom transport
		}

		proxy := httputil.NewSingleHostReverseProxy(target)

		// Custom transport that opens a yamux stream per request
		proxy.Transport = &agentTransport{
			agent:     agent,
			subdomain: subdomain,
			logger:    s.logger,
		}

		// Custom error handler
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			s.logger.Error("proxy error", "subdomain", subdomain, "error", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}

		// Modify the request for forwarding
		proxy.Director = func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "tunnel-agent"

			// Add proxy headers
			req.Header.Set("X-Forwarded-For", clientIP)
			req.Header.Set("X-Forwarded-Proto", proto)
			req.Header.Set("X-Real-IP", clientIP)
			req.Header.Set("X-Tunnel-Subdomain", subdomain)

			// Preserve the original host header
			if req.Header.Get("X-Forwarded-Host") == "" {
				req.Header.Set("X-Forwarded-Host", req.Host)
			}
		}

		// Enable streaming responses with flushing
		proxy.FlushInterval = -1 // Flush immediately

		proxy.ServeHTTP(w, r)
	})
}

// agentTransport implements http.RoundTripper to forward requests over yamux streams
type agentTransport struct {
	agent     *AgentSession
	subdomain string
	logger    *slog.Logger
}

func (t *agentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Open a new yamux stream for this request
	stream, err := t.agent.Session.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}

	// Send tunnel init message to identify which tunnel this is for
	initMsg := protocol.TunnelInit{Subdomain: t.subdomain}
	if err := protocol.WriteMessage(stream, protocol.MsgTunnelInit, initMsg); err != nil {
		stream.Close()
		return nil, fmt.Errorf("failed to send tunnel init: %w", err)
	}

	// Create an HTTP client connection over the yamux stream
	// We use http.ReadResponse and req.Write but with proper lifecycle management
	// The stream will be closed when the response body is closed

	// Write the request
	if err := req.Write(stream); err != nil {
		stream.Close()
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	// Read the response
	// Use a buffered reader for HTTP parsing
	resp, err := http.ReadResponse(bufio.NewReader(stream), req)
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Wrap the response body to close the stream when done
	// This is critical - the stream must stay open until the body is fully read
	resp.Body = &streamClosingBody{
		ReadCloser: resp.Body,
		stream:     stream,
	}

	return resp, nil
}

// streamClosingBody wraps a response body to close the yamux stream when the body is closed
type streamClosingBody struct {
	io.ReadCloser
	stream net.Conn
}

func (b *streamClosingBody) Close() error {
	// Close the original body first
	bodyErr := b.ReadCloser.Close()
	// Then close the stream
	streamErr := b.stream.Close()
	if bodyErr != nil {
		return bodyErr
	}
	return streamErr
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status        string `json:"status"`
	Uptime        string `json:"uptime"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	AgentsCount   int    `json:"agents_count"`
	HTTPTunnels   int    `json:"http_tunnels"`
	TCPTunnels    int    `json:"tcp_tunnels"`
}

func (s *Server) listenHealth(ctx context.Context) {
	addr := fmt.Sprintf(":%d", s.config.HealthPort)
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		httpCount := len(s.httpRegistry)
		tcpCount := len(s.tcpRegistry)

		// Count unique agents
		agents := make(map[string]struct{})
		for _, agent := range s.httpRegistry {
			agents[agent.ID] = struct{}{}
		}
		for _, agent := range s.tcpRegistry {
			agents[agent.ID] = struct{}{}
		}
		s.mu.RUnlock()

		uptime := time.Since(s.startedAt)
		resp := HealthResponse{
			Status:        "ok",
			Uptime:        uptime.Round(time.Second).String(),
			UptimeSeconds: int64(uptime.Seconds()),
			AgentsCount:   len(agents),
			HTTPTunnels:   httpCount,
			TCPTunnels:    tcpCount,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/health", http.StatusFound)
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	s.logger.Info("health endpoint listening", "addr", addr)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("health server error", "error", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdownCtx)
}

func (s *Server) handleAgent(ctx context.Context, conn net.Conn) {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 30 * time.Second
	cfg.ConnectionWriteTimeout = 60 * time.Second // Increase from 10s default
	cfg.MaxStreamWindowSize = 1024 * 1024         // 1MB window (from 256KB)
	session, err := yamux.Server(conn, cfg)
	if err != nil {
		s.logger.Error("yamux setup failed", "error", err)
		conn.Close()
		return
	}

	ctlStream, err := session.Accept()
	if err != nil {
		s.logger.Error("failed to accept control stream", "error", err)
		session.Close()
		return
	}

	msg, ctlReader, err := protocol.ReadMessageBuffered(ctlStream)
	if err != nil {
		s.logger.Error("handshake read failed", "error", err)
		session.Close()
		return
	}

	if msg.Type != protocol.MsgAuthRequest {
		s.logger.Warn("invalid handshake message type", "type", msg.Type)
		session.Close()
		return
	}

	var req protocol.AuthRequest
	if err := json.Unmarshal(msg.Payload, &req); err != nil {
		s.logger.Error("invalid auth payload", "error", err)
		session.Close()
		return
	}

	if req.Token != s.config.AuthToken {
		protocol.WriteMessage(ctlStream, protocol.MsgAuthResponse, protocol.AuthResponse{
			Success: false,
			Error:   "invalid token",
		})
		session.Close()
		return
	}

	var assignedURLs []string
	var agentSubdomains []string
	var agentPorts []int

	s.mu.Lock()

	// Initialize Agent
	agent := &AgentSession{
		ID:          generateSubdomain(),
		Session:     session,
		Conn:        conn,
		ConnectedAt: time.Now(),
	}

	for _, t := range req.Tunnels {
		if t.Type == "http" {
			sub := strings.ToLower(t.Subdomain)
			if sub == "" {
				sub = generateSubdomain()
			} else {
				// Validate user-provided subdomain
				if err := validateSubdomain(sub); err != nil {
					s.mu.Unlock()
					protocol.WriteMessage(ctlStream, protocol.MsgAuthResponse, protocol.AuthResponse{
						Success: false,
						Error:   err.Error(),
					})
					session.Close()
					return
				}
			}
			if _, exists := s.httpRegistry[sub]; exists {
				s.mu.Unlock()
				protocol.WriteMessage(ctlStream, protocol.MsgAuthResponse, protocol.AuthResponse{
					Success: false,
					Error:   fmt.Sprintf("subdomain %s in use", sub),
				})
				session.Close()
				return
			}
			s.httpRegistry[sub] = agent
			agentSubdomains = append(agentSubdomains, sub)

			// Build URL
			url := fmt.Sprintf("%s.%s", sub, s.config.Domain)
			if s.config.TLSEmail != "" {
				url = "https://" + url
			} else if s.config.PublicPort != 80 {
				url = fmt.Sprintf("http://%s:%d", url, s.config.PublicPort)
			} else {
				url = "http://" + url
			}
			assignedURLs = append(assignedURLs, url)

		} else if t.Type == "tcp" {
			port := t.RemotePort

			// Start listener - if port is 0, OS will assign a random available port
			l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
			if err != nil {
				s.mu.Unlock()
				protocol.WriteMessage(ctlStream, protocol.MsgAuthResponse, protocol.AuthResponse{
					Success: false,
					Error:   fmt.Sprintf("failed to bind port %d: %v", port, err),
				})
				session.Close()
				return
			}

			// Get the actual assigned port (important for random port assignment)
			actualPort := l.Addr().(*net.TCPAddr).Port

			// Check if port is already in use in our registry
			if _, exists := s.tcpRegistry[actualPort]; exists {
				l.Close()
				s.mu.Unlock()
				protocol.WriteMessage(ctlStream, protocol.MsgAuthResponse, protocol.AuthResponse{
					Success: false,
					Error:   fmt.Sprintf("port %d in use", actualPort),
				})
				session.Close()
				return
			}

			s.tcpListeners[actualPort] = l
			s.tcpRegistry[actualPort] = agent
			agentPorts = append(agentPorts, actualPort)

			assignedURLs = append(assignedURLs, fmt.Sprintf("tcp://%s:%d", s.config.Domain, actualPort))

			// Handle TCP connections
			go s.handleTCPListener(l, agent, actualPort)
		}
	}

	agent.Subdomains = agentSubdomains
	agent.TCPPorts = agentPorts

	protocol.WriteMessage(ctlStream, protocol.MsgAuthResponse, protocol.AuthResponse{
		Success: true,
		URLs:    assignedURLs,
	})

	s.logger.Info("agent registered", "subdomains", agentSubdomains, "ports", agentPorts, "remote", conn.RemoteAddr())

	s.mu.Unlock()

	// Start control message handler for dynamic tunnel management
	go s.handleControlMessages(ctlStream, ctlReader, agent)

	go func() {
		<-session.CloseChan()
		s.cleanupAgent(agent)
	}()
}

// handleControlMessages handles dynamic tunnel add/remove requests from connected agents
func (s *Server) handleControlMessages(ctlStream net.Conn, reader io.Reader, agent *AgentSession) {
	s.logger.Info("control message handler started", "agent", agent.ID)
	for {
		msg, nextReader, err := protocol.ReadMessageBuffered(reader)
		if err != nil {
			// Stream closed or error - agent disconnected
			s.logger.Info("control message handler exiting", "agent", agent.ID, "error", err)
			return
		}
		reader = nextReader // Reuse the buffered reader

		s.logger.Info("received control message", "type", msg.Type, "agent", agent.ID)

		switch msg.Type {
		case protocol.MsgTunnelAdd:
			s.handleTunnelAdd(ctlStream, agent, msg.Payload)
		case protocol.MsgTunnelRemove:
			s.handleTunnelRemove(ctlStream, agent, msg.Payload)
		default:
			s.logger.Warn("unknown control message type", "type", msg.Type)
		}
	}
}

// handleTunnelAdd processes a dynamic tunnel add request
func (s *Server) handleTunnelAdd(ctlStream net.Conn, agent *AgentSession, payload json.RawMessage) {
	s.logger.Info("processing tunnel add request", "agent", agent.ID)

	var req protocol.TunnelAddRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		s.logger.Error("failed to unmarshal tunnel add request", "error", err)
		protocol.WriteMessage(ctlStream, protocol.MsgTunnelAddResponse, protocol.TunnelAddResponse{
			RequestID: "",
			Success:   false,
			Error:     "invalid request payload",
		})
		return
	}

	s.logger.Info("tunnel add request details", "requestID", req.RequestID, "type", req.Tunnel.Type, "subdomain", req.Tunnel.Subdomain)

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Tunnel.Type == "http" {
		url, err := s.addHTTPTunnelLocked(agent, req.Tunnel.Subdomain)
		if err != nil {
			protocol.WriteMessage(ctlStream, protocol.MsgTunnelAddResponse, protocol.TunnelAddResponse{
				RequestID: req.RequestID,
				Success:   false,
				Error:     err.Error(),
			})
			return
		}
		protocol.WriteMessage(ctlStream, protocol.MsgTunnelAddResponse, protocol.TunnelAddResponse{
			RequestID: req.RequestID,
			Success:   true,
			URL:       url,
		})
		s.logger.Info("tunnel added dynamically", "type", "http", "subdomain", req.Tunnel.Subdomain, "agent", agent.ID)

	} else if req.Tunnel.Type == "tcp" {
		url, port, err := s.addTCPTunnelLocked(agent, req.Tunnel.RemotePort)
		if err != nil {
			protocol.WriteMessage(ctlStream, protocol.MsgTunnelAddResponse, protocol.TunnelAddResponse{
				RequestID: req.RequestID,
				Success:   false,
				Error:     err.Error(),
			})
			return
		}
		protocol.WriteMessage(ctlStream, protocol.MsgTunnelAddResponse, protocol.TunnelAddResponse{
			RequestID:    req.RequestID,
			Success:      true,
			URL:          url,
			AssignedPort: port,
		})
		s.logger.Info("tunnel added dynamically", "type", "tcp", "port", port, "agent", agent.ID)

	} else {
		protocol.WriteMessage(ctlStream, protocol.MsgTunnelAddResponse, protocol.TunnelAddResponse{
			RequestID: req.RequestID,
			Success:   false,
			Error:     fmt.Sprintf("unknown tunnel type: %s", req.Tunnel.Type),
		})
	}
}

// addHTTPTunnelLocked adds an HTTP tunnel (must be called with s.mu held)
func (s *Server) addHTTPTunnelLocked(agent *AgentSession, subdomain string) (string, error) {
	sub := strings.ToLower(subdomain)
	if sub == "" {
		sub = generateSubdomain()
	} else {
		if err := validateSubdomain(sub); err != nil {
			return "", err
		}
	}

	if _, exists := s.httpRegistry[sub]; exists {
		return "", fmt.Errorf("subdomain %s already in use", sub)
	}

	s.httpRegistry[sub] = agent
	agent.Subdomains = append(agent.Subdomains, sub)

	// Build URL
	url := fmt.Sprintf("%s.%s", sub, s.config.Domain)
	if s.config.TLSEmail != "" {
		url = "https://" + url
	} else if s.config.PublicPort != 80 {
		url = fmt.Sprintf("http://%s:%d", url, s.config.PublicPort)
	} else {
		url = "http://" + url
	}

	return url, nil
}

// addTCPTunnelLocked adds a TCP tunnel (must be called with s.mu held)
func (s *Server) addTCPTunnelLocked(agent *AgentSession, requestedPort int) (string, int, error) {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", requestedPort))
	if err != nil {
		return "", 0, fmt.Errorf("failed to bind port %d: %v", requestedPort, err)
	}

	actualPort := l.Addr().(*net.TCPAddr).Port

	if _, exists := s.tcpRegistry[actualPort]; exists {
		l.Close()
		return "", 0, fmt.Errorf("port %d already in use", actualPort)
	}

	s.tcpListeners[actualPort] = l
	s.tcpRegistry[actualPort] = agent
	agent.TCPPorts = append(agent.TCPPorts, actualPort)

	go s.handleTCPListener(l, agent, actualPort)

	url := fmt.Sprintf("tcp://%s:%d", s.config.Domain, actualPort)
	return url, actualPort, nil
}

// handleTunnelRemove processes a dynamic tunnel remove request
func (s *Server) handleTunnelRemove(ctlStream net.Conn, agent *AgentSession, payload json.RawMessage) {
	var req protocol.TunnelRemoveRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		protocol.WriteMessage(ctlStream, protocol.MsgTunnelRemoveResponse, protocol.TunnelRemoveResponse{
			RequestID: "",
			Success:   false,
			Error:     "invalid request payload",
		})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	if req.Type == "http" {
		err = s.removeHTTPTunnelLocked(agent, req.Subdomain)
	} else if req.Type == "tcp" {
		err = s.removeTCPTunnelLocked(agent, req.RemotePort)
	} else {
		err = fmt.Errorf("unknown tunnel type: %s", req.Type)
	}

	if err != nil {
		protocol.WriteMessage(ctlStream, protocol.MsgTunnelRemoveResponse, protocol.TunnelRemoveResponse{
			RequestID: req.RequestID,
			Success:   false,
			Error:     err.Error(),
		})
		return
	}

	protocol.WriteMessage(ctlStream, protocol.MsgTunnelRemoveResponse, protocol.TunnelRemoveResponse{
		RequestID: req.RequestID,
		Success:   true,
	})
	s.logger.Info("tunnel removed dynamically", "type", req.Type, "agent", agent.ID)
}

// removeHTTPTunnelLocked removes an HTTP tunnel (must be called with s.mu held)
func (s *Server) removeHTTPTunnelLocked(agent *AgentSession, subdomain string) error {
	sub := strings.ToLower(subdomain)

	existing, exists := s.httpRegistry[sub]
	if !exists {
		return fmt.Errorf("tunnel not found: %s", sub)
	}
	if existing.ID != agent.ID {
		return fmt.Errorf("tunnel does not belong to this agent")
	}

	delete(s.httpRegistry, sub)

	// Remove from agent's subdomain list
	for i, sd := range agent.Subdomains {
		if sd == sub {
			agent.Subdomains = append(agent.Subdomains[:i], agent.Subdomains[i+1:]...)
			break
		}
	}

	return nil
}

// removeTCPTunnelLocked removes a TCP tunnel (must be called with s.mu held)
func (s *Server) removeTCPTunnelLocked(agent *AgentSession, port int) error {
	existing, exists := s.tcpRegistry[port]
	if !exists {
		return fmt.Errorf("tunnel not found: port %d", port)
	}
	if existing.ID != agent.ID {
		return fmt.Errorf("tunnel does not belong to this agent")
	}

	// Close the listener
	if l, ok := s.tcpListeners[port]; ok {
		l.Close()
		delete(s.tcpListeners, port)
	}

	delete(s.tcpRegistry, port)

	// Remove from agent's port list
	for i, p := range agent.TCPPorts {
		if p == port {
			agent.TCPPorts = append(agent.TCPPorts[:i], agent.TCPPorts[i+1:]...)
			break
		}
	}

	return nil
}

// cleanupAgent removes an agent's tunnels from the registries
func (s *Server) cleanupAgent(agent *AgentSession) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sub := range agent.Subdomains {
		delete(s.httpRegistry, sub)
	}
	for _, port := range agent.TCPPorts {
		delete(s.tcpRegistry, port)
		if l, ok := s.tcpListeners[port]; ok {
			l.Close()
			delete(s.tcpListeners, port)
		}
	}
	s.logger.Info("agent disconnected", "id", agent.ID)
}

func (s *Server) handleTCPListener(l net.Listener, agent *AgentSession, port int) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return // Listener closed
		}
		go s.proxyTCP(conn, agent, port)
	}
}

func (s *Server) proxyTCP(conn net.Conn, agent *AgentSession, port int) {
	// Disable Nagle's algorithm for low-latency interactive sessions (SSH, etc.)
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	stream, err := agent.Session.Open()
	if err != nil {
		conn.Close()
		return
	}

	// Use "tcp:PORT" format to identify TCP tunnels in TunnelInit
	targetID := fmt.Sprintf("tcp:%d", port)

	initMsg := protocol.TunnelInit{Subdomain: targetID}
	if err := protocol.WriteMessage(stream, protocol.MsgTunnelInit, initMsg); err != nil {
		stream.Close()
		conn.Close()
		return
	}

	go func() {
		io.Copy(stream, conn)
		stream.Close()
	}()
	io.Copy(conn, stream)
	conn.Close()
}

// extractClientIP extracts the IP address from a RemoteAddr string (ip:port)
func extractClientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// generateSubdomain creates a cryptographically random subdomain.
func generateSubdomain() string {
	b := make([]byte, 5) // 5 bytes = 8 base32 chars
	if _, err := rand.Read(b); err != nil {
		// Fallback should never happen, but handle it
		return fmt.Sprintf("t%d", time.Now().UnixNano()%1000000)
	}
	// Use base32 without padding, lowercase for DNS compatibility
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// Reserved subdomains that cannot be used
var reservedSubdomains = map[string]bool{
	"www":       true,
	"api":       true,
	"admin":     true,
	"mail":      true,
	"smtp":      true,
	"ftp":       true,
	"ns":        true,
	"ns1":       true,
	"ns2":       true,
	"mx":        true,
	"mx1":       true,
	"mx2":       true,
	"localhost": true,
	"tunnel":    true,
	"health":    true,
	"status":    true,
	"metrics":   true,
}

// validateSubdomain validates a subdomain string
func validateSubdomain(sub string) error {
	// Check length (3-63 characters as per DNS label rules)
	if len(sub) < 3 {
		return fmt.Errorf("subdomain too short: minimum 3 characters")
	}
	if len(sub) > 63 {
		return fmt.Errorf("subdomain too long: maximum 63 characters")
	}

	// Check for reserved names
	if reservedSubdomains[sub] {
		return fmt.Errorf("subdomain %q is reserved", sub)
	}

	// Check that it starts with a letter or number
	if !isAlphanumeric(rune(sub[0])) {
		return fmt.Errorf("subdomain must start with a letter or number")
	}

	// Check that it ends with a letter or number
	if !isAlphanumeric(rune(sub[len(sub)-1])) {
		return fmt.Errorf("subdomain must end with a letter or number")
	}

	// Check all characters (alphanumeric and hyphens only)
	for i, c := range sub {
		if c == '-' {
			// Hyphens are allowed, but not at start or end (already checked)
			continue
		}
		if !isAlphanumeric(c) {
			return fmt.Errorf("subdomain contains invalid character at position %d: only letters, numbers, and hyphens allowed", i)
		}
	}

	// Check for consecutive hyphens (often reserved for special purposes like punycode)
	if strings.Contains(sub, "--") {
		return fmt.Errorf("subdomain cannot contain consecutive hyphens")
	}

	return nil
}

func isAlphanumeric(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
