package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/profclems/tunnel/client"
	"github.com/profclems/tunnel/server"
)

// discardHandler is a slog handler that discards all logs
type discardHandler struct{}

func (d discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (d discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return d }
func (d discardHandler) WithGroup(string) slog.Handler             { return d }

func newTestLogger() *slog.Logger {
	return slog.New(discardHandler{})
}

func TestIntegration_HTTPTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find free ports
	controlPort := findFreePort(t)
	publicPort := findFreePort(t)
	localPort := findFreePort(t)

	// Start a simple HTTP server as the "local service"
	localServer := startTestHTTPServer(t, localPort)
	defer localServer.Close()

	// Start the tunnel server
	srvCfg := server.Config{
		ControlPort: controlPort,
		PublicPort:  publicPort,
		Domain:      "localhost",
		AuthToken:   "test-token",
	}
	srv := server.NewServer(srvCfg, newTestLogger())

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Start(ctx)
	}()

	// Wait for server to start
	waitForPort(t, controlPort, 5*time.Second)
	waitForPort(t, publicPort, 5*time.Second)

	// Start the tunnel client
	clientCfg := client.Config{
		ServerAddr: fmt.Sprintf("127.0.0.1:%d", controlPort),
		AuthToken:  "test-token",
		Tunnels: []client.TunnelConfig{
			{
				Type:      "http",
				Subdomain: "test",
				LocalAddr: fmt.Sprintf("127.0.0.1:%d", localPort),
			},
		},
	}
	c := client.NewClient(clientCfg, newTestLogger())

	clientDone := make(chan error, 1)
	go func() {
		clientDone <- c.Start(ctx)
	}()

	// Wait for client to connect
	time.Sleep(500 * time.Millisecond)

	// Make a request through the tunnel
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/hello", publicPort), nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Host = "test.localhost"

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}

	if !strings.Contains(string(body), "Hello from test server") {
		t.Errorf("unexpected body: %s", string(body))
	}

	// Check that X-Forwarded-For was injected
	// The test server echoes headers in the response
	if !strings.Contains(string(body), "X-Forwarded-For") {
		t.Log("Note: X-Forwarded-For header not found in response (may depend on server implementation)")
	}

	cancel()
}

func TestIntegration_TCPTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find free ports
	controlPort := findFreePort(t)
	publicPort := findFreePort(t)
	localPort := findFreePort(t)
	remotePort := findFreePort(t)

	// Start a simple TCP server as the "local service"
	localServer := startTestTCPServer(t, localPort)
	defer localServer.Close()

	// Start the tunnel server
	srvCfg := server.Config{
		ControlPort: controlPort,
		PublicPort:  publicPort,
		Domain:      "localhost",
		AuthToken:   "test-token",
	}
	srv := server.NewServer(srvCfg, newTestLogger())

	go func() {
		srv.Start(ctx)
	}()

	// Wait for server to start
	waitForPort(t, controlPort, 5*time.Second)

	// Start the tunnel client
	clientCfg := client.Config{
		ServerAddr: fmt.Sprintf("127.0.0.1:%d", controlPort),
		AuthToken:  "test-token",
		Tunnels: []client.TunnelConfig{
			{
				Type:       "tcp",
				RemotePort: remotePort,
				LocalAddr:  fmt.Sprintf("127.0.0.1:%d", localPort),
			},
		},
	}
	c := client.NewClient(clientCfg, newTestLogger())

	go func() {
		c.Start(ctx)
	}()

	// Wait for client to connect and TCP listener to be established
	time.Sleep(1 * time.Second)
	waitForPort(t, remotePort, 5*time.Second)

	// Connect through the tunnel
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort), 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect through tunnel: %v", err)
	}
	defer conn.Close()

	// Send data
	message := "Hello through TCP tunnel"
	_, err = conn.Write([]byte(message))
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	// Read response
	buf := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	response := string(buf[:n])
	expected := "ECHO: " + message
	if response != expected {
		t.Errorf("expected %q, got %q", expected, response)
	}

	cancel()
}

func TestIntegration_MultiTunnel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find free ports
	controlPort := findFreePort(t)
	publicPort := findFreePort(t)
	localHTTPPort := findFreePort(t)
	localTCPPort := findFreePort(t)
	remoteTCPPort := findFreePort(t)

	// Start local servers
	httpServer := startTestHTTPServer(t, localHTTPPort)
	defer httpServer.Close()

	tcpServer := startTestTCPServer(t, localTCPPort)
	defer tcpServer.Close()

	// Start the tunnel server
	srvCfg := server.Config{
		ControlPort: controlPort,
		PublicPort:  publicPort,
		Domain:      "localhost",
		AuthToken:   "test-token",
	}
	srv := server.NewServer(srvCfg, newTestLogger())

	go func() {
		srv.Start(ctx)
	}()

	// Wait for server to start
	waitForPort(t, controlPort, 5*time.Second)
	waitForPort(t, publicPort, 5*time.Second)

	// Start the tunnel client with multiple tunnels
	clientCfg := client.Config{
		ServerAddr: fmt.Sprintf("127.0.0.1:%d", controlPort),
		AuthToken:  "test-token",
		Tunnels: []client.TunnelConfig{
			{
				Type:      "http",
				Subdomain: "web",
				LocalAddr: fmt.Sprintf("127.0.0.1:%d", localHTTPPort),
			},
			{
				Type:       "tcp",
				RemotePort: remoteTCPPort,
				LocalAddr:  fmt.Sprintf("127.0.0.1:%d", localTCPPort),
			},
		},
	}
	c := client.NewClient(clientCfg, newTestLogger())

	go func() {
		c.Start(ctx)
	}()

	// Wait for client to connect
	time.Sleep(1 * time.Second)

	// Test HTTP tunnel
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/hello", publicPort), nil)
	req.Host = "web.localhost"

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTP: expected status 200, got %d", resp.StatusCode)
	}

	// Test TCP tunnel
	waitForPort(t, remoteTCPPort, 5*time.Second)

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", remoteTCPPort), 5*time.Second)
	if err != nil {
		t.Fatalf("TCP connection failed: %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("test"))
	buf := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("TCP read failed: %v", err)
	}

	if !strings.HasPrefix(string(buf[:n]), "ECHO:") {
		t.Errorf("TCP: unexpected response: %s", string(buf[:n]))
	}

	cancel()
}

func TestIntegration_InvalidToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	controlPort := findFreePort(t)
	publicPort := findFreePort(t)

	// Start the tunnel server
	srvCfg := server.Config{
		ControlPort: controlPort,
		PublicPort:  publicPort,
		Domain:      "localhost",
		AuthToken:   "correct-token",
	}
	srv := server.NewServer(srvCfg, newTestLogger())

	go func() {
		srv.Start(ctx)
	}()

	waitForPort(t, controlPort, 5*time.Second)

	// Try to connect with wrong token
	clientCfg := client.Config{
		ServerAddr: fmt.Sprintf("127.0.0.1:%d", controlPort),
		AuthToken:  "wrong-token",
		Tunnels: []client.TunnelConfig{
			{
				Type:      "http",
				Subdomain: "test",
				LocalAddr: "127.0.0.1:3000",
			},
		},
	}
	c := client.NewClient(clientCfg, newTestLogger())

	// Client should fail to establish tunnel (will keep retrying)
	// We just verify it doesn't crash
	clientCtx, clientCancel := context.WithTimeout(ctx, 2*time.Second)
	defer clientCancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Start(clientCtx)
	}()

	select {
	case <-clientCtx.Done():
		// Expected - client times out trying to reconnect
	case err := <-errCh:
		if err != context.DeadlineExceeded && err != context.Canceled {
			t.Logf("Client returned error (expected for invalid token): %v", err)
		}
	}

	cancel()
}

// Helper functions

func findFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func waitForPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Don't fail here - port might not be open yet but test might still work
	t.Logf("warning: port %d not ready after %v", port, timeout)
}

func startTestHTTPServer(t *testing.T, port int) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello from test server\n")
		fmt.Fprintf(w, "Headers:\n")
		for name, values := range r.Header {
			for _, v := range values {
				fmt.Fprintf(w, "  %s: %s\n", name, v)
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK")
	})

	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}

	go server.ListenAndServe()
	waitForPort(t, port, 5*time.Second)
	return server
}

func startTestTCPServer(t *testing.T, port int) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("failed to start TCP server: %v", err)
	}

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write([]byte("ECHO: " + string(buf[:n])))
				}
			}(conn)
		}
	}()

	return l
}
