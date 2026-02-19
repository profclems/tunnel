package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

func newCertCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cert",
		Short: "Certificate management for mTLS",
	}

	cmd.AddCommand(newCertInitCmd())
	cmd.AddCommand(newCertIssueCmd())

	return cmd
}

func newCertInitCmd() *cobra.Command {
	var (
		outDir   string
		validFor int
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize CA and server certificates",
		Long: `Generate a new Certificate Authority (CA) and server certificate for mTLS.

This creates:
  - ca.crt: CA certificate (distribute to clients for verification)
  - ca.key: CA private key (keep secret, used to sign certs)
  - server.crt: Server certificate (for TLS on control port)
  - server.key: Server private key

Example:
  tunnel cert init --out /etc/tunnel/certs`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := os.MkdirAll(outDir, 0700); err != nil {
				return fmt.Errorf("failed to create output directory: %w", err)
			}

			caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return fmt.Errorf("failed to generate CA key: %w", err)
			}

			serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
			if err != nil {
				return fmt.Errorf("failed to generate serial number: %w", err)
			}

			caTemplate := x509.Certificate{
				SerialNumber: serialNumber,
				Subject: pkix.Name{
					Organization: []string{"Tunnel CA"},
					CommonName:   "Tunnel Root CA",
				},
				NotBefore:             time.Now(),
				NotAfter:              time.Now().AddDate(0, 0, validFor),
				KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
				BasicConstraintsValid: true,
				IsCA:                  true,
				MaxPathLen:            1,
			}

			caCertDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
			if err != nil {
				return fmt.Errorf("failed to create CA certificate: %w", err)
			}

			// Write CA cert
			caCertPath := filepath.Join(outDir, "ca.crt")
			caCertFile, err := os.OpenFile(caCertPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				return fmt.Errorf("failed to create CA cert file: %w", err)
			}
			defer caCertFile.Close()

			if err := pem.Encode(caCertFile, &pem.Block{Type: "CERTIFICATE", Bytes: caCertDER}); err != nil {
				return fmt.Errorf("failed to write CA cert: %w", err)
			}

			// Write CA key
			caKeyPath := filepath.Join(outDir, "ca.key")
			caKeyFile, err := os.OpenFile(caKeyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
			if err != nil {
				return fmt.Errorf("failed to create CA key file: %w", err)
			}
			defer caKeyFile.Close()

			caKeyDER, err := x509.MarshalECPrivateKey(caKey)
			if err != nil {
				return fmt.Errorf("failed to marshal CA key: %w", err)
			}

			if err := pem.Encode(caKeyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER}); err != nil {
				return fmt.Errorf("failed to write CA key: %w", err)
			}

			// Now generate server certificate
			serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return fmt.Errorf("failed to generate server key: %w", err)
			}

			serverSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
			if err != nil {
				return fmt.Errorf("failed to generate server serial number: %w", err)
			}

			serverTemplate := x509.Certificate{
				SerialNumber: serverSerial,
				Subject: pkix.Name{
					CommonName:   "Tunnel Server",
					Organization: []string{"Tunnel"},
				},
				NotBefore:   time.Now(),
				NotAfter:    time.Now().AddDate(0, 0, validFor),
				KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
				ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}

			// Parse CA cert for signing
			caCertParsed, err := x509.ParseCertificate(caCertDER)
			if err != nil {
				return fmt.Errorf("failed to parse CA cert: %w", err)
			}

			serverCertDER, err := x509.CreateCertificate(rand.Reader, &serverTemplate, caCertParsed, &serverKey.PublicKey, caKey)
			if err != nil {
				return fmt.Errorf("failed to create server certificate: %w", err)
			}

			// Write server cert
			serverCertPath := filepath.Join(outDir, "server.crt")
			serverCertFile, err := os.OpenFile(serverCertPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				return fmt.Errorf("failed to create server cert file: %w", err)
			}
			defer serverCertFile.Close()

			if err := pem.Encode(serverCertFile, &pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER}); err != nil {
				return fmt.Errorf("failed to write server cert: %w", err)
			}

			// Write server key
			serverKeyPath := filepath.Join(outDir, "server.key")
			serverKeyFile, err := os.OpenFile(serverKeyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
			if err != nil {
				return fmt.Errorf("failed to create server key file: %w", err)
			}
			defer serverKeyFile.Close()

			serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
			if err != nil {
				return fmt.Errorf("failed to marshal server key: %w", err)
			}

			if err := pem.Encode(serverKeyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}); err != nil {
				return fmt.Errorf("failed to write server key: %w", err)
			}

			fmt.Printf("mTLS initialized successfully:\n")
			fmt.Printf("\nCA:\n")
			fmt.Printf("  Certificate: %s\n", caCertPath)
			fmt.Printf("  Private Key: %s (keep secret!)\n", caKeyPath)
			fmt.Printf("\nServer:\n")
			fmt.Printf("  Certificate: %s\n", serverCertPath)
			fmt.Printf("  Private Key: %s\n", serverKeyPath)
			fmt.Printf("\nValid for: %d days\n", validFor)
			fmt.Printf("\nServer config:\n")
			fmt.Printf("  agent_ca: %s\n", caCertPath)
			fmt.Printf("  agent_cert: %s\n", serverCertPath)
			fmt.Printf("  agent_key: %s\n", serverKeyPath)
			fmt.Printf("  require_agent_cert: true\n")
			fmt.Printf("\nIssue client certs:\n")
			fmt.Printf("  tunnel cert issue --name client1 --ca-cert %s --ca-key %s\n", caCertPath, caKeyPath)

			return nil
		},
	}

	cmd.Flags().StringVarP(&outDir, "out", "o", ".", "Output directory for CA files")
	cmd.Flags().IntVar(&validFor, "days", 365, "CA validity period in days")

	return cmd
}

func newCertIssueCmd() *cobra.Command {
	var (
		name     string
		caCert   string
		caKey    string
		outDir   string
		validFor int
	)

	cmd := &cobra.Command{
		Use:   "issue",
		Short: "Issue a client certificate",
		Long: `Issue a new client certificate signed by the CA.

This creates:
  - <name>.crt: Client certificate
  - <name>.key: Client private key

Example:
  tunnel cert issue --name agent1 --ca-cert ca.crt --ca-key ca.key`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}

			// Load CA cert
			caCertPEM, err := os.ReadFile(caCert)
			if err != nil {
				return fmt.Errorf("failed to read CA cert: %w", err)
			}
			caCertBlock, _ := pem.Decode(caCertPEM)
			if caCertBlock == nil {
				return fmt.Errorf("failed to parse CA cert PEM")
			}
			caCertParsed, err := x509.ParseCertificate(caCertBlock.Bytes)
			if err != nil {
				return fmt.Errorf("failed to parse CA cert: %w", err)
			}

			// Load CA key
			caKeyPEM, err := os.ReadFile(caKey)
			if err != nil {
				return fmt.Errorf("failed to read CA key: %w", err)
			}
			caKeyBlock, _ := pem.Decode(caKeyPEM)
			if caKeyBlock == nil {
				return fmt.Errorf("failed to parse CA key PEM")
			}
			caKeyParsed, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
			if err != nil {
				return fmt.Errorf("failed to parse CA key: %w", err)
			}

			// Generate client key
			clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return fmt.Errorf("failed to generate client key: %w", err)
			}

			serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
			if err != nil {
				return fmt.Errorf("failed to generate serial number: %w", err)
			}

			clientTemplate := x509.Certificate{
				SerialNumber: serialNumber,
				Subject: pkix.Name{
					CommonName:   name,
					Organization: []string{"Tunnel Client"},
				},
				NotBefore:   time.Now(),
				NotAfter:    time.Now().AddDate(0, 0, validFor),
				KeyUsage:    x509.KeyUsageDigitalSignature,
				ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			}

			clientCertDER, err := x509.CreateCertificate(rand.Reader, &clientTemplate, caCertParsed, &clientKey.PublicKey, caKeyParsed)
			if err != nil {
				return fmt.Errorf("failed to create client certificate: %w", err)
			}

			if err := os.MkdirAll(outDir, 0700); err != nil {
				return fmt.Errorf("failed to create output directory: %w", err)
			}

			// Write client cert
			clientCertPath := filepath.Join(outDir, name+".crt")
			clientCertFile, err := os.OpenFile(clientCertPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				return fmt.Errorf("failed to create client cert file: %w", err)
			}
			defer clientCertFile.Close()

			if err := pem.Encode(clientCertFile, &pem.Block{Type: "CERTIFICATE", Bytes: clientCertDER}); err != nil {
				return fmt.Errorf("failed to write client cert: %w", err)
			}

			// Write client key
			clientKeyPath := filepath.Join(outDir, name+".key")
			clientKeyFile, err := os.OpenFile(clientKeyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
			if err != nil {
				return fmt.Errorf("failed to create client key file: %w", err)
			}
			defer clientKeyFile.Close()

			clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
			if err != nil {
				return fmt.Errorf("failed to marshal client key: %w", err)
			}

			if err := pem.Encode(clientKeyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER}); err != nil {
				return fmt.Errorf("failed to write client key: %w", err)
			}

			fmt.Printf("Client certificate issued:\n")
			fmt.Printf("  Name:        %s\n", name)
			fmt.Printf("  Certificate: %s\n", clientCertPath)
			fmt.Printf("  Private Key: %s\n", clientKeyPath)
			fmt.Printf("  Valid for:   %d days\n", validFor)
			fmt.Printf("\nClient usage:\n")
			fmt.Printf("  tunnel http 3000 --server example.com:8081 --tls --tls-cert %s --tls-key %s --tls-ca %s\n", clientCertPath, clientKeyPath, caCert)

			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Client name (used as CN and filename)")
	cmd.Flags().StringVar(&caCert, "ca-cert", "ca.crt", "CA certificate file")
	cmd.Flags().StringVar(&caKey, "ca-key", "ca.key", "CA private key file")
	cmd.Flags().StringVarP(&outDir, "out", "o", ".", "Output directory for client files")
	cmd.Flags().IntVar(&validFor, "days", 90, "Certificate validity period in days")

	cmd.MarkFlagRequired("name")

	return cmd
}
