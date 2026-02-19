package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// MTLSConfig holds configuration for mutual TLS
type MTLSConfig struct {
	// CA certificate file for verifying client certificates
	AgentCAFile string
	// Server certificate file (for agent connections)
	AgentCertFile string
	// Server key file (for agent connections)
	AgentKeyFile string
	// Whether to require client certificates
	RequireClientCert bool
}

// CreateAgentTLSConfig creates a TLS config for agent connections with optional mTLS
func CreateAgentTLSConfig(cfg MTLSConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	// Load server certificate if provided
	if cfg.AgentCertFile != "" && cfg.AgentKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.AgentCertFile, cfg.AgentKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load server certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate for client verification
	if cfg.AgentCAFile != "" {
		caCert, err := os.ReadFile(cfg.AgentCAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}

		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.ClientCAs = caPool

		if cfg.RequireClientCert {
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}

	return tlsConfig, nil
}
