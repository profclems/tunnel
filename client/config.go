package client

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// ConfigManager handles persisting tunnel configuration
type ConfigManager struct {
	mu       sync.Mutex
	filePath string
}

// TunnelConfigFile represents the YAML structure
type TunnelConfigFile struct {
	Server       string        `yaml:"server"`
	Token        string        `yaml:"token"`
	Inspect      bool          `yaml:"inspect,omitempty"`
	InspectPort  int           `yaml:"inspect_port,omitempty"`
	Template     string        `yaml:"template,omitempty"`      // Inspector template: minimal|developer|terminal|modern|monitoring
	TemplatePath string        `yaml:"template_path,omitempty"` // Custom template file path
	TLS          bool          `yaml:"tls,omitempty"`
	TLSInsecure  bool          `yaml:"tls_insecure,omitempty"`
	TLSCert      string        `yaml:"tls_cert,omitempty"`
	TLSKey       string        `yaml:"tls_key,omitempty"`
	TLSCA        string        `yaml:"tls_ca,omitempty"`
	Tunnels      []TunnelEntry `yaml:"tunnels"`
}

// TunnelEntry represents a tunnel in the config file
type TunnelEntry struct {
	Type       string `yaml:"type"`
	Subdomain  string `yaml:"subdomain,omitempty"`
	RemotePort int    `yaml:"remote_port,omitempty"`
	Local      string `yaml:"local"`
}

// NewConfigManager creates a config manager
func NewConfigManager(filePath string) *ConfigManager {
	return &ConfigManager{filePath: filePath}
}

// DefaultConfigPath returns the default config file path
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tunnel", "tunnel.yaml")
}

// FilePath returns the config file path
func (cm *ConfigManager) FilePath() string {
	return cm.filePath
}

// Load reads the config file
func (cm *ConfigManager) Load() (*TunnelConfigFile, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	data, err := os.ReadFile(cm.filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg TunnelConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}

// Save writes the config file
func (cm *ConfigManager) Save(cfg *TunnelConfigFile) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	return cm.saveLocked(cfg)
}

func (cm *ConfigManager) saveLocked(cfg *TunnelConfigFile) error {
	// Ensure directory exists
	dir := filepath.Dir(cm.filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write atomically using temp file + rename
	tmpPath := cm.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	if err := os.Rename(tmpPath, cm.filePath); err != nil {
		os.Remove(tmpPath) // Clean up
		return fmt.Errorf("failed to save config: %w", err)
	}

	return nil
}

// AddTunnel adds a tunnel to the config and saves
func (cm *ConfigManager) AddTunnel(entry TunnelEntry) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cfg, err := cm.loadLocked()
	if err != nil {
		return err
	}

	cfg.Tunnels = append(cfg.Tunnels, entry)
	return cm.saveLocked(cfg)
}

// RemoveTunnel removes a tunnel from the config and saves
func (cm *ConfigManager) RemoveTunnel(tunnelType, subdomain string, port int) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cfg, err := cm.loadLocked()
	if err != nil {
		return err
	}

	for i, t := range cfg.Tunnels {
		if t.Type == tunnelType {
			if tunnelType == "http" && t.Subdomain == subdomain {
				cfg.Tunnels = append(cfg.Tunnels[:i], cfg.Tunnels[i+1:]...)
				break
			}
			if tunnelType == "tcp" && t.RemotePort == port {
				cfg.Tunnels = append(cfg.Tunnels[:i], cfg.Tunnels[i+1:]...)
				break
			}
		}
	}

	return cm.saveLocked(cfg)
}

func (cm *ConfigManager) loadLocked() (*TunnelConfigFile, error) {
	data, err := os.ReadFile(cm.filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg TunnelConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}
