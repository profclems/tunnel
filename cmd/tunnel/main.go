package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	version = "dev"
	logger  *slog.Logger
	cfgFile string
)

func main() {
	logger = slog.New(slog.NewTextHandler(os.Stdout, nil))

	rootCmd := &cobra.Command{
		Use:           "tunnel",
		Short:         "A simple, secure tunneling service",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.SetVersionTemplate(fmt.Sprintf("tunnel %s\n", version))
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./tunnel.yaml)")

	cobra.OnInitialize(initConfig)

	rootCmd.AddCommand(newServerCmd())
	rootCmd.AddCommand(newHTTPCmd())
	rootCmd.AddCommand(newTCPCmd())
	rootCmd.AddCommand(newStartCmd())
	rootCmd.AddCommand(newCertCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.AddConfigPath(".")
		viper.SetConfigName("tunnel")
	}

	viper.SetEnvPrefix("TUNNEL")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		logger.Info("using config file", "file", viper.ConfigFileUsed())
	}
}
