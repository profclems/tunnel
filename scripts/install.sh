#!/bin/bash
set -e

# Tunnel Server Installation Script
# Usage: curl -sSL https://raw.githubusercontent.com/profclems/tunnel/main/scripts/install.sh | sudo bash

INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/tunnel"
DATA_DIR="/var/lib/tunnel"
SERVICE_USER="tunnel"
BINARY_NAME="tunnel"
REPO="profclems/tunnel"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log() { echo -e "${GREEN}[+]${NC} $1"; }
warn() { echo -e "${YELLOW}[!]${NC} $1"; }
error() { echo -e "${RED}[x]${NC} $1"; exit 1; }

# Check if running as root
check_root() {
    if [ "$EUID" -ne 0 ]; then
        error "Please run as root or with sudo"
    fi
}

# Detect OS and architecture
detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)

    case "$ARCH" in
        x86_64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) error "Unsupported architecture: $ARCH" ;;
    esac

    case "$OS" in
        linux) ;;
        darwin) ;;
        *) error "Unsupported OS: $OS" ;;
    esac

    PLATFORM="${OS}-${ARCH}"
    log "Detected platform: $PLATFORM"
}

# Build from source using Go
build_from_source() {
    log "Building from source with Go..."
    GOPROXY=direct GOBIN="${INSTALL_DIR}" go install -v "github.com/${REPO}/cmd/tunnel@latest"
}

# Download pre-built binary
download_binary() {
    log "Downloading pre-built binary..."
    DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/tunnel-${PLATFORM}"

    if command -v curl &> /dev/null; then
        curl -fsSL "$DOWNLOAD_URL" -o "/tmp/${BINARY_NAME}" 2>/dev/null || return 1
    elif command -v wget &> /dev/null; then
        wget -q "$DOWNLOAD_URL" -O "/tmp/${BINARY_NAME}" 2>/dev/null || return 1
    else
        return 1
    fi

    chmod +x "/tmp/${BINARY_NAME}"
    mv "/tmp/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
    return 0
}

# Install binary
install_binary() {
    HAS_GO=false
    if command -v go &> /dev/null; then
        HAS_GO=true
    fi

    echo ""
    echo "Installation method:"
    echo "  1) Download pre-built binary"
    if [ "$HAS_GO" = true ]; then
        echo "  2) Build from source (Go $(go version | awk '{print $3}') detected)"
    else
        echo "  2) Build from source (requires Go)"
    fi
    read -p "Choose installation method [1/2]: " INSTALL_METHOD < /dev/tty

    case "$INSTALL_METHOD" in
        2)
            if [ "$HAS_GO" != true ]; then
                error "Go is not installed. Install from https://go.dev"
            fi
            if build_from_source; then
                log "Installed to ${INSTALL_DIR}/${BINARY_NAME}"
                return 0
            fi
            error "Build from source failed"
            ;;
        *)
            detect_platform
            if download_binary; then
                log "Installed to ${INSTALL_DIR}/${BINARY_NAME}"
                return 0
            fi
            error "Failed to download binary. Check GitHub releases."
            ;;
    esac
}

# Create system user
create_user() {
    if id "$SERVICE_USER" &>/dev/null; then
        log "User $SERVICE_USER already exists"
    else
        log "Creating system user: $SERVICE_USER"
        useradd -r -s /bin/false "$SERVICE_USER"
    fi
}

# Create directories
create_dirs() {
    log "Creating directories..."
    mkdir -p "$CONFIG_DIR" "$DATA_DIR"
    # Config dir: root owns, tunnel group can read (for service to read config)
    chown root:"$SERVICE_USER" "$CONFIG_DIR"
    chmod 750 "$CONFIG_DIR"
    # Data dir: tunnel owns (for service to write data)
    chown "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"
}

# Setup mTLS certificates
setup_mtls() {
    CERTS_DIR="${CONFIG_DIR}/certs"
    mkdir -p "$CERTS_DIR"

    log "Initializing CA and server certificate for mTLS..."
    "${INSTALL_DIR}/${BINARY_NAME}" cert init --out "$CERTS_DIR" --days 365 --domain "$DOMAIN"

    chown -R "$SERVICE_USER:$SERVICE_USER" "$CERTS_DIR"
    chmod 700 "$CERTS_DIR"
    chmod 600 "$CERTS_DIR/ca.key"
    chmod 600 "$CERTS_DIR/server.key"

    echo ""
    log "mTLS initialized in $CERTS_DIR"
    echo ""
}

# Issue client certificate
issue_client_cert() {
    local client_name="$1"
    CERTS_DIR="${CONFIG_DIR}/certs"

    if [ ! -f "$CERTS_DIR/ca.crt" ] || [ ! -f "$CERTS_DIR/ca.key" ]; then
        error "CA not initialized. Run install first with mTLS enabled."
    fi

    "${INSTALL_DIR}/${BINARY_NAME}" cert issue \
        --name "$client_name" \
        --ca-cert "$CERTS_DIR/ca.crt" \
        --ca-key "$CERTS_DIR/ca.key" \
        --out "$CERTS_DIR" \
        --days 90

    echo ""
    log "Client certificate created. Copy these files to the client:"
    echo "  - $CERTS_DIR/${client_name}.crt"
    echo "  - $CERTS_DIR/${client_name}.key"
    echo "  - $CERTS_DIR/ca.crt"
    echo ""
}

# Generate config file
generate_config() {
    CONFIG_FILE="${CONFIG_DIR}/server.yaml"

    if [ -f "$CONFIG_FILE" ]; then
        warn "Config file already exists: $CONFIG_FILE"
        return
    fi

    # Prompt for configuration
    echo ""
    read -p "Enter your domain (e.g., px.example.com): " DOMAIN < /dev/tty
    read -p "Enter your email for Let's Encrypt: " EMAIL < /dev/tty

    # Ask about mTLS
    echo ""
    echo "Authentication options:"
    echo "  1) Token-based (simple shared secret)"
    echo "  2) mTLS (certificate-based, more secure)"
    read -p "Choose authentication method [1/2]: " AUTH_METHOD < /dev/tty

    USE_MTLS=false
    if [ "$AUTH_METHOD" = "2" ]; then
        USE_MTLS=true
        setup_mtls
    fi

    # Generate secure token (used as fallback or additional auth)
    TOKEN=$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | xxd -p)

    log "Generating config file..."

    if [ "$USE_MTLS" = true ]; then
        cat > "$CONFIG_FILE" << EOF
# Tunnel Server Configuration
domain: "${DOMAIN}"
token: "${TOKEN}"
email: "${EMAIL}"

# Ports
control_port: 8081
public_port: 80
tls_port: 443
health_port: 8082
admin_port: 8083
metrics_port: 9090

# Rate limiting
rate_limit: 100
rate_burst: 200

# mTLS - require client certificates
agent_ca: "${CONFIG_DIR}/certs/ca.crt"
agent_cert: "${CONFIG_DIR}/certs/server.crt"
agent_key: "${CONFIG_DIR}/certs/server.key"
require_agent_cert: true
EOF
    else
        cat > "$CONFIG_FILE" << EOF
# Tunnel Server Configuration
domain: "${DOMAIN}"
token: "${TOKEN}"
email: "${EMAIL}"

# Ports
control_port: 8081
public_port: 80
tls_port: 443
health_port: 8082
admin_port: 8083
metrics_port: 9090

# Rate limiting
rate_limit: 100
rate_burst: 200
EOF
    fi

    # Set ownership so tunnel user can read the config
    chown root:"$SERVICE_USER" "$CONFIG_FILE"
    chmod 640 "$CONFIG_FILE"

    echo ""
    log "Config file created: $CONFIG_FILE"

    if [ "$USE_MTLS" = true ]; then
        warn "mTLS enabled - clients must use certificates"
        echo ""
        echo "To issue a client certificate:"
        echo "  sudo tunnel cert issue --name <client-name> \\"
        echo "    --ca-cert ${CONFIG_DIR}/certs/ca.crt \\"
        echo "    --ca-key ${CONFIG_DIR}/certs/ca.key \\"
        echo "    --out ${CONFIG_DIR}/certs"
        echo ""
        echo "Or use the shortcut:"
        echo "  curl -sSL .../install.sh | sudo bash -s -- --issue-cert <client-name>"
    else
        warn "Your auth token: $TOKEN"
        warn "Save this token - you'll need it for clients!"
    fi
    echo ""
}

# Install systemd service
install_service() {
    if [ ! -d "/etc/systemd/system" ]; then
        warn "systemd not found, skipping service installation"
        return
    fi

    log "Installing systemd service..."
    cat > /etc/systemd/system/tunnel-server.service << 'EOF'
[Unit]
Description=Tunnel Server
After=network.target

[Service]
Type=simple
User=tunnel
Group=tunnel
WorkingDirectory=/var/lib/tunnel
ExecStart=/usr/local/bin/tunnel server --config /etc/tunnel/server.yaml
Restart=always
RestartSec=5

# Security
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/tunnel

# Allow binding to low ports
AmbientCapabilities=CAP_NET_BIND_SERVICE

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=tunnel-server

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    log "Service installed: tunnel-server"
}

# Start service
start_service() {
    if [ ! -d "/etc/systemd/system" ]; then
        return
    fi

    read -p "Start tunnel server now? [y/N] " -n 1 -r < /dev/tty
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        systemctl enable tunnel-server
        systemctl start tunnel-server
        log "Service started"
        echo ""
        log "Check status: systemctl status tunnel-server"
        log "View logs: journalctl -u tunnel-server -f"
    fi
}

# Print completion message
print_complete() {
    echo ""
    echo "============================================"
    echo -e "${GREEN}Installation complete!${NC}"
    echo "============================================"
    echo ""
    echo "Configuration: ${CONFIG_DIR}/server.yaml"
    echo "Data directory: ${DATA_DIR}"
    echo "Binary: ${INSTALL_DIR}/${BINARY_NAME}"
    echo ""
    echo "Commands:"
    echo "  Start:   systemctl start tunnel-server"
    echo "  Stop:    systemctl stop tunnel-server"
    echo "  Status:  systemctl status tunnel-server"
    echo "  Logs:    journalctl -u tunnel-server -f"
    echo ""

    if [ "$USE_MTLS" = true ]; then
        echo "Client usage (mTLS):"
        echo "  tunnel http 3000 --server ${DOMAIN:-your-domain}:8081 \\"
        echo "    --tls --tls-cert <client>.crt --tls-key <client>.key --tls-ca ca.crt"
        echo ""
        echo "Issue client certs:"
        echo "  sudo $0 --issue-cert <client-name>"
    else
        echo "Client usage:"
        echo "  tunnel http 3000 --server ${DOMAIN:-your-domain}:8081 --token <token> --subdomain myapp"
    fi
    echo ""
}

# Main
main() {
    echo ""
    echo "================================"
    echo "  Tunnel Server Installer"
    echo "================================"
    echo ""

    check_root
    install_binary
    create_user
    create_dirs
    generate_config
    install_service
    start_service
    print_complete
}

# Run with arguments support
case "${1:-}" in
    --uninstall)
        check_root
        log "Uninstalling tunnel server..."
        systemctl stop tunnel-server 2>/dev/null || true
        systemctl disable tunnel-server 2>/dev/null || true
        rm -f /etc/systemd/system/tunnel-server.service
        rm -f "${INSTALL_DIR}/${BINARY_NAME}"
        userdel "$SERVICE_USER" 2>/dev/null || true
        systemctl daemon-reload
        log "Uninstalled. Config and data preserved in ${CONFIG_DIR} and ${DATA_DIR}"
        ;;
    --upgrade)
        check_root
        systemctl stop tunnel-server 2>/dev/null || true
        install_binary
        systemctl start tunnel-server 2>/dev/null || true
        log "Upgraded to latest version"
        ;;
    --issue-cert)
        check_root
        if [ -z "${2:-}" ]; then
            error "Usage: --issue-cert <client-name>"
        fi
        issue_client_cert "$2"
        ;;
    --help|-h)
        echo "Tunnel Server Installation Script"
        echo ""
        echo "Usage:"
        echo "  install.sh              Install tunnel server"
        echo "  install.sh --upgrade    Upgrade to latest version"
        echo "  install.sh --uninstall  Remove tunnel server"
        echo "  install.sh --issue-cert <name>  Issue a client certificate"
        echo ""
        ;;
    *)
        main
        ;;
esac