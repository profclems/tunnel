# Installation Guide

## Server

### Quick Install (Linux)

```bash
curl -sSL https://raw.githubusercontent.com/profclems/tunnel/main/scripts/install.sh | sudo bash
```

The installer will prompt for your domain and email, then:
- Download the latest binary
- Create system user and directories
- Generate `/etc/tunnel/server.yaml` with a secure token
- Install and start the systemd service

### Upgrade

```bash
curl -sSL https://raw.githubusercontent.com/profclems/tunnel/main/scripts/install.sh | sudo bash -s -- --upgrade
```

### Uninstall

```bash
curl -sSL https://raw.githubusercontent.com/profclems/tunnel/main/scripts/install.sh | sudo bash -s -- --uninstall
```

### Manual Install

1. Download the binary:
```bash
curl -L https://github.com/profclems/tunnel/releases/latest/download/tunnel-linux-amd64 -o /usr/local/bin/tunnel
chmod +x /usr/local/bin/tunnel
```

2. Create config file `/etc/tunnel/server.yaml`:
```yaml
domain: "your-domain.com"
token: "your-secure-token"
email: "admin@your-domain.com"

control_port: 8081
public_port: 80
tls_port: 443
health_port: 8082
admin_port: 8083
metrics_port: 9090
```

3. Run:
```bash
tunnel server --config /etc/tunnel/server.yaml
```

### Docker

```bash
docker run -d \
  -p 80:80 -p 443:443 -p 8081:8081 \
  -v $(pwd)/server.yaml:/etc/tunnel/server.yaml \
  ghcr.io/profclems/tunnel:latest server --config /etc/tunnel/server.yaml
```

---

## Client

### Install

```bash
# macOS (Apple Silicon)
curl -L https://github.com/profclems/tunnel/releases/latest/download/tunnel-darwin-arm64 -o tunnel

# macOS (Intel)
curl -L https://github.com/profclems/tunnel/releases/latest/download/tunnel-darwin-amd64 -o tunnel

# Linux
curl -L https://github.com/profclems/tunnel/releases/latest/download/tunnel-linux-amd64 -o tunnel

chmod +x tunnel
sudo mv tunnel /usr/local/bin/
```

### Quick Start

```bash
# Expose local port 3000
tunnel http 3000 --server your-domain.com:8081 --token your-token --subdomain myapp
```

### Config File

Create `~/tunnel.yaml`:
```yaml
server: "your-domain.com:8081"
token: "your-token"
inspect: true
inspect_port: 4040

# Optional: Custom dashboard template (path to HTML file)
# template_path: "./my-custom-template.html"

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
```

Run:
```bash
tunnel start
```

### Run as Service (macOS)

Create `~/Library/LaunchAgents/com.tunnel.client.plist`:
```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.tunnel.client</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/tunnel</string>
        <string>start</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/tunnel.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/tunnel.log</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/com.tunnel.client.plist
```

### Run as Service (Linux)

```bash
sudo tee /etc/systemd/system/tunnel-client.service << 'EOF'
[Unit]
Description=Tunnel Client
After=network.target

[Service]
Type=simple
User=your-username
ExecStart=/usr/local/bin/tunnel start --config /home/your-username/tunnel.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable tunnel-client
sudo systemctl start tunnel-client
```

---

## DNS Setup

Add these records to your DNS provider:

```
your-domain.com       A    <SERVER_IP>
*.your-domain.com     A    <SERVER_IP>
```

---

## Firewall

Open required ports on your server:

```bash
# All ports (simple)
sudo ufw allow 80,443,8081,8082,8083,9090/tcp
sudo ufw allow 2222:2300/tcp

# Or with GCP
gcloud compute firewall-rules create tunnel \
    --allow tcp:80,tcp:443,tcp:8081,tcp:8082,tcp:8083,tcp:9090,tcp:2222-2300
```

---

## Verify

```bash
# Health check
curl http://your-domain.com:8082/health

# Admin stats
curl http://your-domain.com:8083/api/stats
```