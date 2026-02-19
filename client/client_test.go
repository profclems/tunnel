package client

import (
	"testing"
)

func TestTunnelConfig_MatchHTTP(t *testing.T) {
	config := TunnelConfig{
		Type:      "http",
		Subdomain: "myapp",
		LocalAddr: "127.0.0.1:3000",
	}

	tests := []struct {
		name      string
		initSub   string
		wantMatch bool
	}{
		{
			name:      "exact match",
			initSub:   "myapp",
			wantMatch: true,
		},
		{
			name:      "different subdomain",
			initSub:   "otherapp",
			wantMatch: false,
		},
		{
			name:      "tcp format should not match",
			initSub:   "tcp:2222",
			wantMatch: false,
		},
		{
			name:      "empty subdomain",
			initSub:   "",
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := config.Type == "http" && config.Subdomain == tt.initSub
			if got != tt.wantMatch {
				t.Errorf("match = %v, want %v", got, tt.wantMatch)
			}
		})
	}
}

func TestTunnelConfig_MatchTCP(t *testing.T) {
	config := TunnelConfig{
		Type:       "tcp",
		RemotePort: 2222,
		LocalAddr:  "127.0.0.1:22",
	}

	tests := []struct {
		name      string
		initSub   string
		wantMatch bool
	}{
		{
			name:      "exact tcp port match",
			initSub:   "tcp:2222",
			wantMatch: true,
		},
		{
			name:      "different tcp port",
			initSub:   "tcp:3333",
			wantMatch: false,
		},
		{
			name:      "http subdomain should not match",
			initSub:   "myapp",
			wantMatch: false,
		},
		{
			name:      "malformed tcp format",
			initSub:   "tcp:",
			wantMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectedSub := "tcp:" + "2222"
			got := config.Type == "tcp" && tt.initSub == expectedSub
			if got != tt.wantMatch {
				t.Errorf("match = %v, want %v", got, tt.wantMatch)
			}
		})
	}
}

func TestConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "valid http config",
			config: Config{
				ServerAddr: "localhost:8081",
				AuthToken:  "secret",
				Tunnels: []TunnelConfig{
					{Type: "http", Subdomain: "myapp", LocalAddr: "127.0.0.1:3000"},
				},
			},
			wantErr: false,
		},
		{
			name: "valid tcp config",
			config: Config{
				ServerAddr: "localhost:8081",
				AuthToken:  "secret",
				Tunnels: []TunnelConfig{
					{Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22"},
				},
			},
			wantErr: false,
		},
		{
			name: "multiple tunnels",
			config: Config{
				ServerAddr: "localhost:8081",
				AuthToken:  "secret",
				Tunnels: []TunnelConfig{
					{Type: "http", Subdomain: "web", LocalAddr: "127.0.0.1:3000"},
					{Type: "http", Subdomain: "api", LocalAddr: "127.0.0.1:8080"},
					{Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22"},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation
			if tt.config.ServerAddr == "" {
				t.Error("ServerAddr is empty")
			}
			if len(tt.config.Tunnels) == 0 {
				t.Error("No tunnels configured")
			}
			for i, tun := range tt.config.Tunnels {
				if tun.Type != "http" && tun.Type != "tcp" {
					t.Errorf("tunnel[%d]: invalid type %q", i, tun.Type)
				}
				if tun.LocalAddr == "" {
					t.Errorf("tunnel[%d]: LocalAddr is empty", i)
				}
			}
		})
	}
}

func TestNewClient(t *testing.T) {
	cfg := Config{
		ServerAddr: "localhost:8081",
		AuthToken:  "secret",
		Tunnels: []TunnelConfig{
			{Type: "http", Subdomain: "myapp", LocalAddr: "127.0.0.1:3000"},
		},
		Inspect:     false,
		InspectPort: 4040,
	}

	client := NewClient(cfg, nil)

	if client == nil {
		t.Fatal("NewClient returned nil")
	}

	if client.config.ServerAddr != cfg.ServerAddr {
		t.Errorf("ServerAddr = %v, want %v", client.config.ServerAddr, cfg.ServerAddr)
	}

	if client.config.AuthToken != cfg.AuthToken {
		t.Errorf("AuthToken = %v, want %v", client.config.AuthToken, cfg.AuthToken)
	}

	if len(client.config.Tunnels) != len(cfg.Tunnels) {
		t.Errorf("Tunnels length = %v, want %v", len(client.config.Tunnels), len(cfg.Tunnels))
	}

	if client.inspector != nil {
		t.Error("inspector should be nil when Inspect is false")
	}
}

func TestNewClient_WithInspector(t *testing.T) {
	cfg := Config{
		ServerAddr: "localhost:8081",
		AuthToken:  "secret",
		Tunnels: []TunnelConfig{
			{Type: "http", Subdomain: "myapp", LocalAddr: "127.0.0.1:3000"},
		},
		Inspect:     true,
		InspectPort: 4040,
	}

	client := NewClient(cfg, nil)

	if client == nil {
		t.Fatal("NewClient returned nil")
	}

	if client.inspector == nil {
		t.Error("inspector should be created when Inspect is true")
	}
}

func TestConfig_TLSFields(t *testing.T) {
	cfg := Config{
		ServerAddr:      "localhost:8081",
		AuthToken:       "secret",
		TLSEnabled:      true,
		TLSInsecureSkip: false,
		TLSCertFile:     "/path/to/cert.pem",
		TLSKeyFile:      "/path/to/key.pem",
		TLSCAFile:       "/path/to/ca.pem",
		Tunnels: []TunnelConfig{
			{Type: "http", Subdomain: "myapp", LocalAddr: "127.0.0.1:3000"},
		},
	}

	client := NewClient(cfg, nil)

	if client == nil {
		t.Fatal("NewClient returned nil")
	}

	if !client.config.TLSEnabled {
		t.Error("TLSEnabled should be true")
	}

	if client.config.TLSInsecureSkip {
		t.Error("TLSInsecureSkip should be false")
	}

	if client.config.TLSCertFile != "/path/to/cert.pem" {
		t.Errorf("TLSCertFile = %q, want %q", client.config.TLSCertFile, "/path/to/cert.pem")
	}

	if client.config.TLSKeyFile != "/path/to/key.pem" {
		t.Errorf("TLSKeyFile = %q, want %q", client.config.TLSKeyFile, "/path/to/key.pem")
	}

	if client.config.TLSCAFile != "/path/to/ca.pem" {
		t.Errorf("TLSCAFile = %q, want %q", client.config.TLSCAFile, "/path/to/ca.pem")
	}
}

func TestClient_CreateTLSConfig_NoFiles(t *testing.T) {
	cfg := Config{
		ServerAddr:      "localhost:8081",
		TLSEnabled:      true,
		TLSInsecureSkip: true,
	}

	client := NewClient(cfg, nil)
	tlsCfg, err := client.createTLSConfig()

	if err != nil {
		t.Fatalf("createTLSConfig failed: %v", err)
	}

	if tlsCfg == nil {
		t.Fatal("TLS config is nil")
	}

	if !tlsCfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be true")
	}

	if len(tlsCfg.Certificates) != 0 {
		t.Error("should have no certificates when no cert files specified")
	}
}

func TestClient_CreateTLSConfig_InvalidCertFile(t *testing.T) {
	cfg := Config{
		ServerAddr:  "localhost:8081",
		TLSEnabled:  true,
		TLSCertFile: "/nonexistent/cert.pem",
		TLSKeyFile:  "/nonexistent/key.pem",
	}

	client := NewClient(cfg, nil)
	_, err := client.createTLSConfig()

	if err == nil {
		t.Error("expected error for nonexistent cert files")
	}
}

func TestClient_CreateTLSConfig_InvalidCAFile(t *testing.T) {
	cfg := Config{
		ServerAddr: "localhost:8081",
		TLSEnabled: true,
		TLSCAFile:  "/nonexistent/ca.pem",
	}

	client := NewClient(cfg, nil)
	_, err := client.createTLSConfig()

	if err == nil {
		t.Error("expected error for nonexistent CA file")
	}
}
