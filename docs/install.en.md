# HexClaw Installation & Deployment Guide

**English | [中文](install.md)**

## Table of Contents

- [System Requirements](#system-requirements)
- [Installation Methods](#installation-methods)
- [Configuration](#configuration)
- [Deployment Methods](#deployment-methods)
- [Platform Integration](#platform-integration)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

---

## System Requirements

| Item | Minimum | Recommended |
|------|---------|-------------|
| OS | Linux / macOS / Windows | Linux (Ubuntu 22.04+) |
| Go | >= 1.25 | Latest stable |
| Memory | 128 MB | 512 MB+ |
| Disk | 100 MB | 1 GB+ (including knowledge base data) |
| Network | Access to LLM API | Low-latency connection |

HexClaw compiles to a single binary with no external runtime dependencies (SQLite uses a pure Go implementation).

---

## Installation Methods

### Method 1: go install (recommended for developers)

```bash
go install github.com/hexagon-codes/hexclaw/cmd/hexclaw@latest
```

Verify installation:

```bash
hexclaw version
# HexClaw v0.1.1
```

### Method 2: Build from source

```bash
git clone https://github.com/hexagon-codes/hexclaw.git
cd hexclaw
go build -o hexclaw ./cmd/hexclaw/

# Optional: build with version info
go build -ldflags "-X main.version=v0.1.1 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o hexclaw ./cmd/hexclaw/

# Move to PATH
sudo mv hexclaw /usr/local/bin/
```

### Method 3: Pre-built binary

Download the binary for your platform from [GitHub Releases](https://github.com/hexagon-codes/hexclaw/releases):

```bash
# Linux amd64
curl -sSL https://github.com/hexagon-codes/hexclaw/releases/latest/download/hexclaw-linux-amd64.tar.gz | tar xz
sudo mv hexclaw /usr/local/bin/

# macOS arm64 (Apple Silicon)
curl -sSL https://github.com/hexagon-codes/hexclaw/releases/latest/download/hexclaw-darwin-arm64.tar.gz | tar xz
sudo mv hexclaw /usr/local/bin/
```

### Method 4: Docker

```bash
# Use official image
docker run -d \
  --name hexclaw \
  -p 6060:6060 \
  -e DEEPSEEK_API_KEY="sk-xxx" \
  -v hexclaw-data:/root/.hexclaw \
  ghcr.io/hexagon-codes/hexclaw:latest

# Or build from source
docker build -t hexclaw .
docker run -d --name hexclaw -p 6060:6060 -e DEEPSEEK_API_KEY="sk-xxx" hexclaw
```

---

## Configuration

### Initialize Configuration

```bash
hexclaw init
# Generates: ~/.hexclaw/hexclaw.yaml
```

The default config directory is `~/.hexclaw/`, containing:

```
~/.hexclaw/
├── hexclaw.yaml     # Main config file
├── data.db          # SQLite database (auto-created)
├── memory/          # File memory directory
│   ├── MEMORY.md    # Long-term memory
│   └── YYYY-MM-DD.md  # Daily journal (named by date)
└── skills/          # Skill installation directory
```

### Environment Variables

All sensitive config values should be set via environment variables:

```bash
# LLM API Keys (set at least one)
export DEEPSEEK_API_KEY="sk-xxx"
export OPENAI_API_KEY="sk-xxx"
export ANTHROPIC_API_KEY="sk-xxx"

# Platform tokens (as needed)
export TELEGRAM_BOT_TOKEN="xxx"
export DISCORD_BOT_TOKEN="xxx"
export SLACK_BOT_TOKEN="xoxb-xxx"
export SLACK_SIGNING_SECRET="xxx"
export FEISHU_APP_ID="cli_xxx"
export FEISHU_APP_SECRET="xxx"
```

Reference environment variables in the config file using `${VAR_NAME}`:

```yaml
llm:
  providers:
    deepseek:
      api_key: ${DEEPSEEK_API_KEY}
```

### Configuration Priority

From highest to lowest:

1. Command-line arguments (`--feishu-app-id`)
2. Environment variables (`DEEPSEEK_API_KEY`)
3. Config file (`hexclaw.yaml`)
4. Secure defaults

### Minimal Configuration

Only one LLM API key is needed to start:

```bash
export DEEPSEEK_API_KEY="sk-xxx"
hexclaw serve
```

All security options are enabled by default. SQLite storage is created automatically.

### Full Configuration Example

See the complete config file in [README.en.md](../README.en.md#configuration).

---

## Deployment Methods

### 1. Direct Run (development/testing)

```bash
hexclaw serve
hexclaw serve --config /path/to/hexclaw.yaml
```

### 2. systemd Service (recommended for Linux production)

Create service file `/etc/systemd/system/hexclaw.service`:

```ini
[Unit]
Description=HexClaw AI Agent
After=network.target

[Service]
Type=simple
User=hexclaw
Group=hexclaw
WorkingDirectory=/opt/hexclaw
ExecStart=/usr/local/bin/hexclaw serve --config /opt/hexclaw/hexclaw.yaml
Restart=always
RestartSec=5

# Environment variables
EnvironmentFile=/opt/hexclaw/.env

# Security hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/opt/hexclaw

# Resource limits
LimitNOFILE=65536
MemoryMax=1G

[Install]
WantedBy=multi-user.target
```

Create environment variables file `/opt/hexclaw/.env`:

```bash
DEEPSEEK_API_KEY=sk-xxx
# OPENAI_API_KEY=sk-xxx
```

Start the service:

```bash
# Create user
sudo useradd -r -s /bin/false hexclaw
sudo mkdir -p /opt/hexclaw
sudo chown hexclaw:hexclaw /opt/hexclaw

# Copy config and binary
sudo cp hexclaw /usr/local/bin/
sudo cp hexclaw.yaml /opt/hexclaw/

# Start
sudo systemctl daemon-reload
sudo systemctl enable hexclaw
sudo systemctl start hexclaw

# Check status
sudo systemctl status hexclaw
sudo journalctl -u hexclaw -f
```

### 3. Docker Compose (recommended for containerized deployment)

Create `docker-compose.yml`:

```yaml
version: "3.8"

services:
  hexclaw:
    image: ghcr.io/hexagon-codes/hexclaw:latest
    # Or use local build
    # build: .
    container_name: hexclaw
    restart: unless-stopped
    ports:
      - "6060:6060"
    environment:
      - DEEPSEEK_API_KEY=${DEEPSEEK_API_KEY}
      # - OPENAI_API_KEY=${OPENAI_API_KEY}
      # - TELEGRAM_BOT_TOKEN=${TELEGRAM_BOT_TOKEN}
    volumes:
      - hexclaw-data:/root/.hexclaw
      - ./hexclaw.yaml:/root/.hexclaw/hexclaw.yaml:ro
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:6060/health"]
      interval: 30s
      timeout: 5s
      retries: 3

volumes:
  hexclaw-data:
```

```bash
# Start
docker compose up -d

# View logs
docker compose logs -f hexclaw

# Stop
docker compose down
```

### 4. Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hexclaw
spec:
  replicas: 1
  selector:
    matchLabels:
      app: hexclaw
  template:
    metadata:
      labels:
        app: hexclaw
    spec:
      containers:
        - name: hexclaw
          image: ghcr.io/hexagon-codes/hexclaw:latest
          ports:
            - containerPort: 6060
          env:
            - name: DEEPSEEK_API_KEY
              valueFrom:
                secretKeyRef:
                  name: hexclaw-secrets
                  key: deepseek-api-key
          volumeMounts:
            - name: data
              mountPath: /root/.hexclaw
            - name: config
              mountPath: /root/.hexclaw/hexclaw.yaml
              subPath: hexclaw.yaml
          livenessProbe:
            httpGet:
              path: /health
              port: 6060
            initialDelaySeconds: 10
            periodSeconds: 30
          resources:
            requests:
              memory: "128Mi"
              cpu: "100m"
            limits:
              memory: "512Mi"
              cpu: "500m"
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: hexclaw-data
        - name: config
          configMap:
            name: hexclaw-config
---
apiVersion: v1
kind: Service
metadata:
  name: hexclaw
spec:
  selector:
    app: hexclaw
  ports:
    - port: 6060
      targetPort: 6060
  type: ClusterIP
```

Create Secret and ConfigMap:

```bash
# Create secret
kubectl create secret generic hexclaw-secrets \
  --from-literal=deepseek-api-key=sk-xxx

# Create config
kubectl create configmap hexclaw-config \
  --from-file=hexclaw.yaml

# Deploy
kubectl apply -f hexclaw-k8s.yaml
```

---

## Platform Integration

### Web UI

Enabled by default. Access at `http://127.0.0.1:6060` after startup.

```yaml
platforms:
  web:
    enabled: true
```

### Feishu Bot

1. Create an app on [Feishu Open Platform](https://open.feishu.cn/), get App ID and App Secret
2. Configure event subscription URL: `http://YOUR_HOST:6061/feishu/webhook` (or use CLI args)
3. Enable in config:

```yaml
platforms:
  feishu:
    enabled: true
    app_id: ${FEISHU_APP_ID}
    app_secret: ${FEISHU_APP_SECRET}
    verification_token: ${FEISHU_VERIFICATION_TOKEN}
```

Or via CLI:

```bash
hexclaw serve --feishu-app-id cli_xxx --feishu-app-secret xxx
```

### Telegram Bot

1. Create a bot via [@BotFather](https://t.me/BotFather), get the token
2. Enable in config:

```yaml
platforms:
  telegram:
    enabled: true
    token: ${TELEGRAM_BOT_TOKEN}
```

Telegram uses long polling — no public IP required.

### Discord Bot

1. Create an application on [Discord Developer Portal](https://discord.com/developers/applications)
2. Get Bot Token, enable Message Content Intent
3. Enable in config:

```yaml
platforms:
  discord:
    enabled: true
    token: ${DISCORD_BOT_TOKEN}
```

### Slack Bot

1. Create an app on [Slack API](https://api.slack.com/apps)
2. Configure Events Request URL: `http://YOUR_HOST:6063/slack/events`
3. Subscribe to `message.im` event
4. Enable in config:

```yaml
platforms:
  slack:
    enabled: true
    token: ${SLACK_BOT_TOKEN}
    signing_secret: ${SLACK_SIGNING_SECRET}
```

### DingTalk Bot

1. Create an internal app on [DingTalk Open Platform](https://open-dev.dingtalk.com/)
2. Configure message receive URL: `http://YOUR_HOST:6062/dingtalk/webhook`
3. Enable in config:

```yaml
platforms:
  dingtalk:
    enabled: true
    app_key: ${DINGTALK_APP_KEY}
    app_secret: ${DINGTALK_APP_SECRET}
    robot_code: ${DINGTALK_ROBOT_CODE}
```

### WeCom (Enterprise WeChat)

1. Create an internal app in [WeCom Admin Console](https://work.weixin.qq.com/)
2. Configure message receive URL: `http://YOUR_HOST:6064/wecom/callback`
3. Enable in config:

```yaml
platforms:
  wecom:
    enabled: true
    corp_id: ${WECOM_CORP_ID}
    agent_id: ${WECOM_AGENT_ID}
    secret: ${WECOM_SECRET}
    token: ${WECOM_TOKEN}
    aes_key: ${WECOM_AES_KEY}
```

### WeChat Official Account

1. Configure server on [WeChat Official Platform](https://mp.weixin.qq.com/)
2. Server URL: `http://YOUR_HOST:6065/wechat/callback`
3. Enable in config:

```yaml
platforms:
  wechat:
    enabled: true
    app_id: ${WECHAT_APP_ID}
    app_secret: ${WECHAT_APP_SECRET}
    token: ${WECHAT_TOKEN}
    aes_key: ${WECHAT_AES_KEY}
```

---

## Operations

### Health Check

```bash
curl http://127.0.0.1:6060/health
# {"status":"healthy"}
```

### Logs

HexClaw uses standard `log` output to stderr, manageable via system log tools:

```bash
# systemd
journalctl -u hexclaw -f

# Docker
docker logs -f hexclaw

# Redirect to file
hexclaw serve 2>&1 | tee /var/log/hexclaw.log
```

Real-time logs also available via API:

```bash
# Query logs (supports level/source/keyword filtering)
curl -H "Authorization: Bearer TOKEN" \
  "http://127.0.0.1:6060/api/v1/logs?level=error&limit=50"

# WebSocket real-time stream
wscat -H "Authorization: Bearer TOKEN" \
  -c "ws://127.0.0.1:6060/api/v1/logs/stream"
```

### Data Backup

SQLite database and memory files are located in `~/.hexclaw/`:

```bash
# Backup
tar czf hexclaw-backup-$(date +%Y%m%d).tar.gz ~/.hexclaw/

# Restore
tar xzf hexclaw-backup-20260318.tar.gz -C ~/
```

### Security Audit

Run security audit periodically to check configuration security:

```bash
hexclaw security audit
```

Audit checks:
- Config file permissions
- API key exposure
- Network exposure risks
- Security options status
- Tool permissions
- Cost budget settings
- Sandbox configuration

### Upgrade

```bash
# go install method
go install github.com/hexagon-codes/hexclaw/cmd/hexclaw@latest

# Binary replacement
wget https://github.com/hexagon-codes/hexclaw/releases/latest/download/hexclaw-linux-amd64.tar.gz
tar xzf hexclaw-linux-amd64.tar.gz
sudo mv hexclaw /usr/local/bin/
sudo systemctl restart hexclaw

# Docker
docker compose pull
docker compose up -d
```

Config files are backward-compatible — upgrades typically require no config changes.

---

## Troubleshooting

### Startup Failures

**"No LLM provider available"**

At least one LLM API key must be set:

```bash
export DEEPSEEK_API_KEY="sk-xxx"
```

**"Failed to initialize storage"**

Check data directory permissions:

```bash
ls -la ~/.hexclaw/
# Ensure current user has read/write permissions
chmod 700 ~/.hexclaw/
```

**Port already in use**

Change the listen port:

```yaml
server:
  port: 7070  # Change to another port
```

### Connection Issues

**LLM API timeout**

Check network connectivity, or configure a proxy:

```bash
export HTTPS_PROXY="http://proxy:8080"
hexclaw serve
```

For users with restricted access to certain APIs, configure an API relay:

```yaml
llm:
  providers:
    openai:
      api_key: ${OPENAI_API_KEY}
      base_url: https://your-proxy.com/v1  # API relay URL
      model: gpt-4o
```

**Platform webhook not receiving messages**

1. Confirm the server is publicly accessible
2. Check firewall rules for the relevant port
3. Use ngrok or frp for intranet tunneling (development stage):

```bash
ngrok http 6060
# Use the HTTPS URL provided by ngrok to configure the webhook
```

### Performance Issues

**Slow responses**

- Enable semantic cache to reduce duplicate requests:
  ```yaml
  llm:
    cache:
      enabled: true
      ttl: 24h
  ```
- Use lower-cost models for simple tasks
- Check knowledge base size, adjust `chunk_size` and `top_k` as needed

**High memory usage**

- Reduce the number of messages retained in context:
  ```yaml
  compaction:
    enabled: true
    max_messages: 30
    keep_recent: 5
  ```
- Reduce semantic cache entry count:
  ```yaml
  llm:
    cache:
      max_entries: 1000
  ```
