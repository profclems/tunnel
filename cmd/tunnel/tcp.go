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

func newTCPCmd() *cobra.Command {
	var (
		serverAddr string
		token      string
		remotePort int

		// TLS options
		tlsEnabled      bool
		tlsInsecureSkip bool
		tlsCertFile     string
		tlsKeyFile      string
		tlsCAFile       string
	)

	cmd := &cobra.Command{
		Use:   "tcp <local-port>",
		Short: "Expose a local TCP port to the internet",
		Long: `Expose a local TCP port to the internet via a remote port on the tunnel server.

Examples:
  # Expose local SSH (port 22) on remote port 2222
  tunnel tcp 22 --server example.com:8081 --remote-port 2222

  # Expose local database on random remote port
  tunnel tcp 5432 --server example.com:8081 --remote-port 0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Viper Fallback
			if serverAddr == "" {
				serverAddr = viper.GetString("server")
			}
			if token == "" {
				token = viper.GetString("token")
			}
			if !cmd.Flags().Changed("remote-port") && viper.IsSet("remote-port") {
				remotePort = viper.GetInt("remote-port")
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
						Type:       "tcp",
						RemotePort: remotePort,
						LocalAddr:  localAddr,
					},
				},
				Inspect:         false, // TCP tunnels don't support inspection
				InspectPort:     0,
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
	cmd.Flags().IntVar(&remotePort, "remote-port", 0, "Remote port to expose on (0 for random)")

	// TLS flags
	cmd.Flags().BoolVar(&tlsEnabled, "tls", false, "Enable TLS for server connection")
	cmd.Flags().BoolVar(&tlsInsecureSkip, "tls-insecure", false, "Skip server certificate verification")
	cmd.Flags().StringVar(&tlsCertFile, "tls-cert", "", "Client certificate file (for mTLS)")
	cmd.Flags().StringVar(&tlsKeyFile, "tls-key", "", "Client key file (for mTLS)")
	cmd.Flags().StringVar(&tlsCAFile, "tls-ca", "", "CA certificate for server verification")

	viper.BindPFlags(cmd.Flags())

	return cmd
}
