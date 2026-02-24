package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/profclems/tunnel/client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newHTTPCmd() *cobra.Command {
	var (
		serverAddr  string
		token       string
		subdomain   string
		inspect     bool
		inspectPort int

		// Inspector template options
		template     string
		templatePath string

		// TLS options
		tlsEnabled      bool
		tlsInsecureSkip bool
		tlsCertFile     string
		tlsKeyFile      string
		tlsCAFile       string

		// Basic auth
		basicAuth string
	)

	cmd := &cobra.Command{
		Use:   "http <port>",
		Short: "Expose a local port to the internet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Viper Fallback
			if serverAddr == "" {
				serverAddr = viper.GetString("server")
			}
			if token == "" {
				token = viper.GetString("token")
			}
			if subdomain == "" {
				subdomain = viper.GetString("subdomain")
			}
			// Note: 'inspect' and 'inspectPort' are local to this command usually, but we can bind them
			if !cmd.Flags().Changed("inspect") && viper.IsSet("inspect") {
				inspect = viper.GetBool("inspect")
			}
			if !cmd.Flags().Changed("inspect-port") && viper.IsSet("inspect-port") {
				inspectPort = viper.GetInt("inspect-port")
			}
			if !cmd.Flags().Changed("tls") && viper.IsSet("tls") {
				tlsEnabled = viper.GetBool("tls")
			}
			if !cmd.Flags().Changed("tls-insecure") && viper.IsSet("tls-insecure") {
				tlsInsecureSkip = viper.GetBool("tls-insecure")
			}
			if tlsCertFile == "" {
				tlsCertFile = viper.GetString("tls-cert")
			}
			if tlsKeyFile == "" {
				tlsKeyFile = viper.GetString("tls-key")
			}
			if tlsCAFile == "" {
				tlsCAFile = viper.GetString("tls-ca")
			}
			if template == "" {
				template = viper.GetString("template")
			}
			if templatePath == "" {
				templatePath = viper.GetString("template-path")
			}
			if basicAuth == "" {
				basicAuth = viper.GetString("basic-auth")
			}

			localPort := args[0]
			localAddr := localPort

			// If just a number, assume localhost
			if _, err := strconv.Atoi(localPort); err == nil {
				localAddr = fmt.Sprintf("127.0.0.1:%s", localPort)
			} else if !strings.Contains(localPort, ":") {
				// Invalid format
				return fmt.Errorf("invalid local address or port: %s", localPort)
			}

			cfg := client.Config{
				ServerAddr: serverAddr,
				AuthToken:  token,
				Tunnels: []client.TunnelConfig{
					{
						Type:      "http",
						Subdomain: subdomain,
						LocalAddr: localAddr,
						BasicAuth: basicAuth,
					},
				},
				Inspect:         inspect,
				InspectPort:     inspectPort,
				Template:        template,
				TemplatePath:    templatePath,
				TLSEnabled:      tlsEnabled,
				TLSInsecureSkip: tlsInsecureSkip,
				TLSCertFile:     tlsCertFile,
				TLSKeyFile:      tlsKeyFile,
				TLSCAFile:       tlsCAFile,
			}

			c := client.NewClient(cfg, logger)

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

	cmd.Flags().StringVar(&serverAddr, "server", "127.0.0.1:8081", "Tunnel server address (host:port)")
	cmd.Flags().StringVar(&token, "token", "secret", "Auth token")
	cmd.Flags().StringVar(&subdomain, "subdomain", "", "Requested subdomain (random if empty)")
	cmd.Flags().BoolVar(&inspect, "inspect", false, "Enable request inspection dashboard")
	cmd.Flags().IntVar(&inspectPort, "inspect-port", 4040, "Inspection dashboard port")
	cmd.Flags().StringVar(&template, "template", "developer", "Dashboard template: only developer available")
	cmd.Flags().StringVar(&templatePath, "template-path", "", "Path to custom template file (overrides --template)")
	cmd.Flags().StringVar(&basicAuth, "basic-auth", "", "Basic auth credentials (user:password)")

	// TLS flags
	cmd.Flags().BoolVar(&tlsEnabled, "tls", false, "Enable TLS for server connection")
	cmd.Flags().BoolVar(&tlsInsecureSkip, "tls-insecure", false, "Skip server certificate verification")
	cmd.Flags().StringVar(&tlsCertFile, "tls-cert", "", "Client certificate file (for mTLS)")
	cmd.Flags().StringVar(&tlsKeyFile, "tls-key", "", "Client key file (for mTLS)")
	cmd.Flags().StringVar(&tlsCAFile, "tls-ca", "", "CA certificate for server verification")

	viper.BindPFlags(cmd.Flags())

	return cmd
}
