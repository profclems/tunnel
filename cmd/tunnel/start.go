package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/profclems/tunnel/client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// TunnelConfigYAML represents a single tunnel in the config file
type TunnelConfigYAML struct {
	Type       string `mapstructure:"type"`
	Subdomain  string `mapstructure:"subdomain"`
	RemotePort int    `mapstructure:"remote_port"`
	Local      string `mapstructure:"local"`
}

// StartConfigYAML represents the full config file structure for multi-tunnel
type StartConfigYAML struct {
	Server      string             `mapstructure:"server"`
	Token       string             `mapstructure:"token"`
	Inspect     bool               `mapstructure:"inspect"`
	InspectPort int                `mapstructure:"inspect_port"`
	Tunnels     []TunnelConfigYAML `mapstructure:"tunnels"`

	// TLS options
	TLS         bool   `mapstructure:"tls"`
	TLSInsecure bool   `mapstructure:"tls_insecure"`
	TLSCert     string `mapstructure:"tls_cert"`
	TLSKey      string `mapstructure:"tls_key"`
	TLSCA       string `mapstructure:"tls_ca"`
}

func newStartCmd() *cobra.Command {
	var (
		configFile  string
		inspect     bool
		inspectPort int
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start multiple tunnels from a config file",
		Long: `Start multiple tunnels defined in a YAML configuration file.

Example config file (tunnel.yaml):
  server: "example.com:8081"
  token: "secret"
  inspect: true
  inspect_port: 4040
  tunnels:
    - type: http
      subdomain: web
      local: "127.0.0.1:3000"
    - type: http
      subdomain: api
      local: "127.0.0.1:8080"
    - type: tcp
      remote_port: 2222
      local: "127.0.0.1:22"

Usage:
  tunnel start
  tunnel start --config /path/to/tunnel.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load config
			v := viper.New()

			if configFile != "" {
				v.SetConfigFile(configFile)
			} else {
				// Look for config in home directory first, then current directory
				home, _ := os.UserHomeDir()
				if home != "" {
					v.AddConfigPath(home)
				}
				v.AddConfigPath(".")
				v.SetConfigName("tunnel")
			}

			if err := v.ReadInConfig(); err != nil {
				return fmt.Errorf("failed to read config file: %w", err)
			}

			logger.Info("using config file", "file", v.ConfigFileUsed())

			var cfg StartConfigYAML
			if err := v.Unmarshal(&cfg); err != nil {
				return fmt.Errorf("failed to parse config file: %w", err)
			}

			if cfg.Server == "" {
				return fmt.Errorf("server address is required in config file")
			}

			if len(cfg.Tunnels) == 0 {
				return fmt.Errorf("at least one tunnel is required in config file")
			}

			// Override inspect settings from CLI flags if provided
			if cmd.Flags().Changed("inspect") {
				cfg.Inspect = inspect
			}
			if cmd.Flags().Changed("inspect-port") {
				cfg.InspectPort = inspectPort
			}

			// Default inspect port
			if cfg.InspectPort == 0 {
				cfg.InspectPort = 4040
			}

			// Convert to client config
			var tunnels []client.TunnelConfig
			for i, t := range cfg.Tunnels {
				if t.Type == "" {
					return fmt.Errorf("tunnel[%d]: type is required (http or tcp)", i)
				}
				if t.Local == "" {
					return fmt.Errorf("tunnel[%d]: local address is required", i)
				}
				if t.Type != "http" && t.Type != "tcp" {
					return fmt.Errorf("tunnel[%d]: invalid type %q (must be http or tcp)", i, t.Type)
				}
				if t.Type == "tcp" && t.RemotePort == 0 {
					// Allow 0 for random port assignment
				}

				tunnels = append(tunnels, client.TunnelConfig{
					Type:       t.Type,
					Subdomain:  t.Subdomain,
					RemotePort: t.RemotePort,
					LocalAddr:  t.Local,
				})
			}

			clientCfg := client.Config{
				ServerAddr:      cfg.Server,
				AuthToken:       cfg.Token,
				Tunnels:         tunnels,
				Inspect:         cfg.Inspect,
				InspectPort:     cfg.InspectPort,
				TLSEnabled:      cfg.TLS,
				TLSInsecureSkip: cfg.TLSInsecure,
				TLSCertFile:     cfg.TLSCert,
				TLSKeyFile:      cfg.TLSKey,
				TLSCAFile:       cfg.TLSCA,
			}

			c := client.NewClient(clientCfg, logger)

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			// Handle interrupt signals
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigCh
				logger.Info("shutting down...")
				cancel()
			}()

			return c.Start(ctx)
		},
	}

	cmd.Flags().StringVarP(&configFile, "config", "c", "", "Path to config file (default: ./tunnel.yaml)")
	cmd.Flags().BoolVar(&inspect, "inspect", false, "Enable request inspection dashboard")
	cmd.Flags().IntVar(&inspectPort, "inspect-port", 4040, "Inspection dashboard port")

	return cmd
}
