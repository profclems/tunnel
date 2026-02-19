package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/profclems/tunnel/server"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// ServerConfigYAML represents the server config file structure
type ServerConfigYAML struct {
	Domain      string `mapstructure:"domain"`
	Token       string `mapstructure:"token"`
	Email       string `mapstructure:"email"`
	ControlPort int    `mapstructure:"control_port"`
	PublicPort  int    `mapstructure:"public_port"`
	TLSPort     int    `mapstructure:"tls_port"`
	HealthPort  int    `mapstructure:"health_port"`
	AdminPort   int    `mapstructure:"admin_port"`
	MetricsPort int    `mapstructure:"metrics_port"`
	RateLimit   int    `mapstructure:"rate_limit"`
	RateBurst   int    `mapstructure:"rate_burst"`

	// mTLS options
	AgentCA          string `mapstructure:"agent_ca"`
	AgentCert        string `mapstructure:"agent_cert"`
	AgentKey         string `mapstructure:"agent_key"`
	RequireAgentCert bool   `mapstructure:"require_agent_cert"`
}

func newServerCmd() *cobra.Command {
	var (
		configFile       string
		controlPort      int
		publicPort       int
		tlsPort          int
		healthPort       int
		adminPort        int
		metricsPort      int
		rateLimit        int
		rateBurst        int
		domain           string
		token            string
		email            string
		agentCAFile      string
		agentCertFile    string
		agentKeyFile     string
		requireAgentCert bool
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start the tunnel server",
		Long: `Start the tunnel server with optional YAML configuration.

Example config file (/etc/tunnel/server.yaml):
  domain: "px.example.com"
  token: "your-secure-token"
  email: "admin@example.com"
  control_port: 8081
  public_port: 80
  tls_port: 443
  health_port: 8082
  admin_port: 8083
  metrics_port: 9090
  rate_limit: 100
  rate_burst: 200

Usage:
  tunnel server --config /etc/tunnel/server.yaml
  tunnel server --domain px.example.com --token secret`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfg ServerConfigYAML
			var configDir string

			// Load config file if specified or look for default locations
			if configFile != "" {
				v := viper.New()
				v.SetConfigFile(configFile)
				if err := v.ReadInConfig(); err != nil {
					return fmt.Errorf("failed to read config file: %w", err)
				}
				configDir = filepath.Dir(v.ConfigFileUsed())
				logger.Info("using config file", "file", v.ConfigFileUsed())
				if err := v.Unmarshal(&cfg); err != nil {
					return fmt.Errorf("failed to parse config file: %w", err)
				}
			} else {
				// Try default locations
				v := viper.New()
				v.SetConfigName("server")
				v.SetConfigType("yaml")
				v.AddConfigPath("/etc/tunnel")
				v.AddConfigPath(".")
				if err := v.ReadInConfig(); err == nil {
					configDir = filepath.Dir(v.ConfigFileUsed())
					logger.Info("using config file", "file", v.ConfigFileUsed())
					if err := v.Unmarshal(&cfg); err != nil {
						return fmt.Errorf("failed to parse config file: %w", err)
					}
				}
			}

			// CLI flags override config file values
			if cmd.Flags().Changed("domain") || cfg.Domain == "" {
				if domain != "" {
					cfg.Domain = domain
				} else if cfg.Domain == "" {
					cfg.Domain = "localhost"
				}
			}
			if cmd.Flags().Changed("token") || cfg.Token == "" {
				if token != "" {
					cfg.Token = token
				} else if cfg.Token == "" {
					cfg.Token = "secret"
				}
			}
			if cmd.Flags().Changed("email") {
				cfg.Email = email
			}
			if cmd.Flags().Changed("control-port") || cfg.ControlPort == 0 {
				if controlPort != 0 {
					cfg.ControlPort = controlPort
				} else if cfg.ControlPort == 0 {
					cfg.ControlPort = 8081
				}
			}
			if cmd.Flags().Changed("public-port") || cfg.PublicPort == 0 {
				if publicPort != 0 {
					cfg.PublicPort = publicPort
				} else if cfg.PublicPort == 0 {
					cfg.PublicPort = 80
				}
			}
			if cmd.Flags().Changed("tls-port") || cfg.TLSPort == 0 {
				if tlsPort != 0 {
					cfg.TLSPort = tlsPort
				} else if cfg.TLSPort == 0 {
					cfg.TLSPort = 443
				}
			}
			if cmd.Flags().Changed("health-port") {
				cfg.HealthPort = healthPort
			}
			if cmd.Flags().Changed("admin-port") {
				cfg.AdminPort = adminPort
			}
			if cmd.Flags().Changed("metrics-port") {
				cfg.MetricsPort = metricsPort
			}
			if cmd.Flags().Changed("rate-limit") {
				cfg.RateLimit = rateLimit
			}
			if cmd.Flags().Changed("rate-burst") {
				cfg.RateBurst = rateBurst
			}
			if cmd.Flags().Changed("agent-ca") {
				cfg.AgentCA = agentCAFile
			}
			if cmd.Flags().Changed("agent-cert") {
				cfg.AgentCert = agentCertFile
			}
			if cmd.Flags().Changed("agent-key") {
				cfg.AgentKey = agentKeyFile
			}
			if cmd.Flags().Changed("require-agent-cert") {
				cfg.RequireAgentCert = requireAgentCert
			}

			// Default cert paths relative to config directory
			if cfg.RequireAgentCert && cfg.AgentCA == "" && configDir != "" {
				defaultCA := filepath.Join(configDir, "certs", "ca.crt")
				if _, err := os.Stat(defaultCA); err == nil {
					cfg.AgentCA = defaultCA
					logger.Info("using default CA certificate", "path", defaultCA)
				}
			}

			serverCfg := server.Config{
				ControlPort:      cfg.ControlPort,
				PublicPort:       cfg.PublicPort,
				TLSPort:          cfg.TLSPort,
				HealthPort:       cfg.HealthPort,
				MetricsPort:      cfg.MetricsPort,
				RateLimit:        cfg.RateLimit,
				RateBurst:        cfg.RateBurst,
				Domain:           cfg.Domain,
				AuthToken:        cfg.Token,
				TLSEmail:         cfg.Email,
				AgentCAFile:      cfg.AgentCA,
				AgentCertFile:    cfg.AgentCert,
				AgentKeyFile:     cfg.AgentKey,
				RequireAgentCert: cfg.RequireAgentCert,
			}

			srv := server.NewServer(serverCfg, logger)

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigCh
				logger.Info("shutting down...")
				cancel()
			}()

			if cfg.AdminPort > 0 {
				go srv.ServeAdmin(ctx, cfg.AdminPort)
			}

			return srv.Start(ctx)
		},
	}

	cmd.Flags().StringVarP(&configFile, "config", "c", "", "Path to config file (default: /etc/tunnel/server.yaml)")
	cmd.Flags().IntVar(&controlPort, "control-port", 0, "Port for tunnel agents (default 8081)")
	cmd.Flags().IntVar(&publicPort, "public-port", 0, "Public HTTP port (default 80)")
	cmd.Flags().IntVar(&tlsPort, "tls-port", 0, "Public HTTPS port (default 443)")
	cmd.Flags().IntVar(&healthPort, "health-port", 0, "Health check endpoint port (0 to disable)")
	cmd.Flags().IntVar(&adminPort, "admin-port", 0, "Admin API port (0 to disable)")
	cmd.Flags().IntVar(&metricsPort, "metrics-port", 0, "Metrics endpoint port (0 to disable)")
	cmd.Flags().IntVar(&rateLimit, "rate-limit", 0, "Requests per second per subdomain (0 to disable)")
	cmd.Flags().IntVar(&rateBurst, "rate-burst", 0, "Burst capacity for rate limiting")
	cmd.Flags().StringVar(&domain, "domain", "", "Base domain for tunnels")
	cmd.Flags().StringVar(&token, "token", "", "Auth token for agents")
	cmd.Flags().StringVar(&email, "email", "", "Email for Let's Encrypt (enables HTTPS)")
	cmd.Flags().StringVar(&agentCAFile, "agent-ca", "", "CA certificate for verifying agent client certificates")
	cmd.Flags().StringVar(&agentCertFile, "agent-cert", "", "Server certificate for agent TLS connections")
	cmd.Flags().StringVar(&agentKeyFile, "agent-key", "", "Server key for agent TLS connections")
	cmd.Flags().BoolVar(&requireAgentCert, "require-agent-cert", false, "Require client certificates from agents")

	return cmd
}
