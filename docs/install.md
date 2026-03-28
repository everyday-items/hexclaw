# HexClaw 安装与部署指南

**[English](install.en.md) | 中文**

## 目录

- [系统要求](#系统要求)
- [安装方式](#安装方式)
- [配置](#配置)
- [部署方式](#部署方式)
- [平台接入](#平台接入)
- [运维](#运维)
- [故障排查](#故障排查)

---

## 系统要求

| 项目 | 最低要求 | 推荐 |
|------|---------|------|
| 操作系统 | Linux / macOS / Windows | Linux (Ubuntu 22.04+) |
| Go | >= 1.25 | 最新稳定版 |
| 内存 | 128 MB | 512 MB+ |
| 磁盘 | 100 MB | 1 GB+（含知识库数据） |
| 网络 | 可访问 LLM API | 低延迟连接 |

HexClaw 编译为单二进制文件，无外部运行时依赖（SQLite 使用纯 Go 实现）。

---

## 安装方式

### 方式一：go install（推荐开发者使用）

```bash
go install github.com/hexagon-codes/hexclaw/cmd/hexclaw@latest
```

验证安装：

```bash
hexclaw version
# HexClaw v0.0.2
```

### 方式二：从源码编译

```bash
git clone https://github.com/hexagon-codes/hexclaw.git
cd hexclaw
go build -o hexclaw ./cmd/hexclaw/

# 可选：带版本信息编译
go build -ldflags "-X main.version=v0.0.2 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o hexclaw ./cmd/hexclaw/

# 移动到 PATH
sudo mv hexclaw /usr/local/bin/
```

### 方式三：预编译二进制

从 [GitHub Releases](https://github.com/hexagon-codes/hexclaw/releases) 下载对应平台的二进制：

```bash
# Linux amd64
curl -sSL https://github.com/hexagon-codes/hexclaw/releases/latest/download/hexclaw-linux-amd64.tar.gz | tar xz
sudo mv hexclaw /usr/local/bin/

# macOS arm64 (Apple Silicon)
curl -sSL https://github.com/hexagon-codes/hexclaw/releases/latest/download/hexclaw-darwin-arm64.tar.gz | tar xz
sudo mv hexclaw /usr/local/bin/
```

### 方式四：Docker

```bash
# 使用官方镜像
docker run -d \
  --name hexclaw \
  -p 16060:16060 \
  -e DEEPSEEK_API_KEY="sk-xxx" \
  -v hexclaw-data:/data/.hexclaw \
  ghcr.io/hexagon-codes/hexclaw:latest

# 或从源码构建
docker build -t hexclaw .
docker run -d --name hexclaw -p 16060:16060 -e DEEPSEEK_API_KEY="sk-xxx" hexclaw
```

---

## 配置

### 初始化配置

```bash
hexclaw init
# 生成: ~/.hexclaw/hexclaw.yaml
```

默认配置目录为 `~/.hexclaw/`，包含以下文件：

```
~/.hexclaw/
├── hexclaw.yaml     # 主配置文件
├── data.db          # SQLite 数据库（自动创建）
├── memory/          # 文件记忆目录
│   ├── MEMORY.md    # 长期记忆
│   └── YYYY-MM-DD.md  # 每日日记（以当天日期命名）
└── skills/          # 技能安装目录
```

### 技能市场（hexclaw-hub）

桌面端「技能市场」从在线目录拉取 `index.json` 与 `skills/*.md`。默认仓库为 **`hexagon-codes/hexclaw-hub`** 的 **`v0.0.2`** 标签。自建镜像时可覆盖：

```yaml
skills:
  enabled: true
  hub:
    repo_url: https://github.com/hexagon-codes/hexclaw-hub
    branch: v0.0.2
```

安装或卸载 Markdown 技能后，引擎会**自动同步**运行时技能注册表，一般**无需重启** sidecar。
通过 API 在线安装时，`source` 可写为 `clawhub://skill-name`；`GET /api/v1/clawhub/search` 支持 `q` 和 `category` 参数。

### 环境变量

所有敏感配置建议通过环境变量设置：

```bash
# LLM API Key（至少设置一个）
export DEEPSEEK_API_KEY="sk-xxx"
export OPENAI_API_KEY="sk-xxx"
export ANTHROPIC_API_KEY="sk-xxx"

# 平台 Token（按需设置）
export TELEGRAM_BOT_TOKEN="xxx"
export DISCORD_BOT_TOKEN="xxx"
export SLACK_BOT_TOKEN="xoxb-xxx"
export SLACK_SIGNING_SECRET="xxx"
export FEISHU_APP_ID="cli_xxx"
export FEISHU_APP_SECRET="xxx"
```

配置文件中使用 `${VAR_NAME}` 引用环境变量：

```yaml
llm:
  providers:
    deepseek:
      api_key: ${DEEPSEEK_API_KEY}
```

### 配置优先级

从高到低：

1. 命令行参数（`--feishu-app-id`）
2. 环境变量（`DEEPSEEK_API_KEY`）
3. 配置文件（`hexclaw.yaml`）
4. 安全默认值

### 最小配置

只需一个 LLM API Key 即可启动：

```bash
export DEEPSEEK_API_KEY="sk-xxx"
hexclaw serve
```

所有安全选项默认开启，存储使用 SQLite 自动创建。

### 完整配置示例

参考 [README.md](../README.md#配置) 中的完整配置文件。

---

## 部署方式

### 1. 直接运行（开发/测试）

```bash
hexclaw serve
hexclaw serve --config /path/to/hexclaw.yaml
```

### 2. systemd 服务（Linux 生产部署推荐）

创建服务文件 `/etc/systemd/system/hexclaw.service`：

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

# 环境变量
EnvironmentFile=/opt/hexclaw/.env

# 安全加固
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/opt/hexclaw

# 资源限制
LimitNOFILE=65536
MemoryMax=1G

[Install]
WantedBy=multi-user.target
```

创建环境变量文件 `/opt/hexclaw/.env`：

```bash
DEEPSEEK_API_KEY=sk-xxx
# OPENAI_API_KEY=sk-xxx
```

启动服务：

```bash
# 创建用户
sudo useradd -r -s /bin/false hexclaw
sudo mkdir -p /opt/hexclaw
sudo chown hexclaw:hexclaw /opt/hexclaw

# 复制配置和二进制
sudo cp hexclaw /usr/local/bin/
sudo cp hexclaw.yaml /opt/hexclaw/

# 启动
sudo systemctl daemon-reload
sudo systemctl enable hexclaw
sudo systemctl start hexclaw

# 查看状态
sudo systemctl status hexclaw
sudo journalctl -u hexclaw -f
```

### 3. Docker Compose（容器化部署推荐）

创建 `docker-compose.yml`：

```yaml
version: "3.8"

services:
  hexclaw:
    image: ghcr.io/hexagon-codes/hexclaw:latest
    # 或使用本地构建
    # build: .
    container_name: hexclaw
    restart: unless-stopped
    ports:
      - "16060:16060"
    environment:
      - DEEPSEEK_API_KEY=${DEEPSEEK_API_KEY}
      # - OPENAI_API_KEY=${OPENAI_API_KEY}
      # - TELEGRAM_BOT_TOKEN=${TELEGRAM_BOT_TOKEN}
    volumes:
      - hexclaw-data:/data/.hexclaw
      - ./hexclaw.yaml:/data/.hexclaw/hexclaw.yaml:ro
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:16060/health"]
      interval: 30s
      timeout: 5s
      retries: 3

volumes:
  hexclaw-data:
```

```bash
# 启动
docker compose up -d

# 查看日志
docker compose logs -f hexclaw

# 停止
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
            - containerPort: 16060
          env:
            - name: DEEPSEEK_API_KEY
              valueFrom:
                secretKeyRef:
                  name: hexclaw-secrets
                  key: deepseek-api-key
          volumeMounts:
            - name: data
              mountPath: /data/.hexclaw
            - name: config
              mountPath: /data/.hexclaw/hexclaw.yaml
              subPath: hexclaw.yaml
          livenessProbe:
            httpGet:
              path: /health
              port: 16060
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
    - port: 16060
      targetPort: 16060
  type: ClusterIP
```

创建 Secret 和 ConfigMap：

```bash
# 创建密钥
kubectl create secret generic hexclaw-secrets \
  --from-literal=deepseek-api-key=sk-xxx

# 创建配置
kubectl create configmap hexclaw-config \
  --from-file=hexclaw.yaml

# 部署
kubectl apply -f hexclaw-k8s.yaml
```

---

## 平台接入

### Web UI

默认启用。启动后访问 `http://127.0.0.1:16060`。

```yaml
platforms:
  web:
    enabled: true
```

### 飞书 Bot

1. 在[飞书开放平台](https://open.feishu.cn/)创建应用，获取 App ID 和 App Secret
2. 配置事件订阅 URL: `http://YOUR_HOST:6061/feishu/webhook`（或使用命令行参数）
3. 启用配置：

```yaml
platforms:
  feishu:
    enabled: true
    app_id: ${FEISHU_APP_ID}
    app_secret: ${FEISHU_APP_SECRET}
    verification_token: ${FEISHU_VERIFICATION_TOKEN}
```

或通过命令行参数：

```bash
hexclaw serve --feishu-app-id cli_xxx --feishu-app-secret xxx
```

### Telegram Bot

1. 通过 [@BotFather](https://t.me/BotFather) 创建 Bot，获取 Token
2. 启用配置：

```yaml
platforms:
  telegram:
    enabled: true
    token: ${TELEGRAM_BOT_TOKEN}
```

Telegram 使用长轮询模式，无需公网 IP。

### Discord Bot

1. 在 [Discord Developer Portal](https://discord.com/developers/applications) 创建应用
2. 获取 Bot Token，启用 Message Content Intent
3. 启用配置：

```yaml
platforms:
  discord:
    enabled: true
    token: ${DISCORD_BOT_TOKEN}
```

### Slack Bot

1. 在 [Slack API](https://api.slack.com/apps) 创建应用
2. 配置 Events Request URL: `http://YOUR_HOST:6063/slack/events`
3. 订阅 `message.im` 事件
4. 启用配置：

```yaml
platforms:
  slack:
    enabled: true
    token: ${SLACK_BOT_TOKEN}
    signing_secret: ${SLACK_SIGNING_SECRET}
```

### 钉钉 Bot

1. 在[钉钉开放平台](https://open-dev.dingtalk.com/)创建企业内部应用
2. 配置消息接收地址: `http://YOUR_HOST:6062/dingtalk/webhook`
3. 启用配置：

```yaml
platforms:
  dingtalk:
    enabled: true
    app_key: ${DINGTALK_APP_KEY}
    app_secret: ${DINGTALK_APP_SECRET}
    robot_code: ${DINGTALK_ROBOT_CODE}
```

### 企业微信

1. 在[企业微信管理后台](https://work.weixin.qq.com/)创建自建应用
2. 配置接收消息 URL: `http://YOUR_HOST:6064/wecom/callback`
3. 启用配置：

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

### 微信公众号

1. 在[微信公众平台](https://mp.weixin.qq.com/)配置服务器
2. 服务器 URL: `http://YOUR_HOST:6065/wechat/callback`
3. 启用配置：

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

## 运维

### 健康检查

```bash
curl http://127.0.0.1:16060/health
# {"status":"healthy"}
```

### 启动后联调检查

如果配置了 `server.api_token`，下面所有写请求都需要追加 `Authorization: Bearer <TOKEN>`。

```bash
# 1. 真实测试 LLM 配置，不写入磁盘
curl -X POST http://127.0.0.1:16060/api/v1/config/llm/test \
  -H "Content-Type: application/json" \
  -d '{
    "provider": {
      "type": "openai",
      "base_url": "https://api.openai.com/v1",
      "api_key": "sk-xxx",
      "model": "gpt-4o-mini"
    }
  }'

# 1b. 本地 Ollama 连通性测试可不填 api_key
curl -X POST http://127.0.0.1:16060/api/v1/config/llm/test \
  -H "Content-Type: application/json" \
  -d '{
    "provider": {
      "type": "ollama",
      "base_url": "http://127.0.0.1:11434/v1",
      "api_key": "",
      "model": "llama3.1"
    }
  }'

# 2. 搜索并在线安装 ClawHub 技能
curl "http://127.0.0.1:16060/api/v1/clawhub/search?q=calendar&category=automation"
curl -X POST http://127.0.0.1:16060/api/v1/skills/install \
  -H "Content-Type: application/json" \
  -d '{"source":"clawhub://<SKILL_NAME>"}'

# 3. 检查 Skills 运行态字段
curl http://127.0.0.1:16060/api/v1/skills
curl -X PUT http://127.0.0.1:16060/api/v1/skills/example/status \
  -H "Content-Type: application/json" \
  -d '{"enabled":true}'

# 4. 查看 Cron 历史结果字段
curl http://127.0.0.1:16060/api/v1/cron/jobs/<JOB_ID>/history

# 5. 验证知识库结构化搜索
curl -X POST http://127.0.0.1:16060/api/v1/knowledge/search \
  -H "Content-Type: application/json" \
  -d '{"query":"RAG","limit":5}'

# 6. 检查平台实例与 IM 通道测试接口
curl http://127.0.0.1:16060/api/v1/platforms/instances
curl -X POST http://127.0.0.1:16060/api/v1/im/channels/telegram/test \
  -H "Content-Type: application/json" \
  -d '{"token":"123:abc"}'
```

重点返回语义：

- `/api/v1/config/llm/test` 返回 `ok/message/provider/model/latency_ms`；当 `provider.type=ollama` 时允许空 `api_key` 做本地连通性测试
- `/api/v1/skills` 和 `/api/v1/skills/{name}/status` 返回 `enabled/effective_enabled/requires_restart/message`
- `/api/v1/skills/install` 支持 `clawhub://skill-name` 或本地相对路径；在线安装成功时返回 `requires_restart=false/runtime_registered=true`
- `/api/v1/cron/jobs/{id}/history` 的历史项包含 `result`，便于直接展示执行输出摘要
- `/api/v1/knowledge/search` 返回结构化 chunk 结果（`result` 和 `results` 字段均为 `[]SearchHit` 数组），不再只是拼接后的上下文字符串
- `/api/v1/knowledge/documents` 返回 `status/error_message/updated_at/source_type`
- `/api/v1/knowledge/documents/{id}` 返回单个文档的完整内容
- `/api/v1/logs` 支持 `domain` 过滤，便于按功能域诊断问题

### 日志

HexClaw 使用标准 `log` 输出到 stderr，可通过系统日志工具管理：

```bash
# systemd
journalctl -u hexclaw -f

# Docker
docker logs -f hexclaw

# 重定向到文件
hexclaw serve 2>&1 | tee /var/log/hexclaw.log
```

实时日志也可通过 API 获取：

```bash
# 查询日志（支持 level/source/domain/keyword 过滤）
curl -H "Authorization: Bearer TOKEN" \
  "http://127.0.0.1:16060/api/v1/logs?domain=knowledge&level=error&limit=50"

# WebSocket 实时流
wscat -H "Authorization: Bearer TOKEN" \
  -c "ws://127.0.0.1:16060/api/v1/logs/stream"
```

### 数据备份

SQLite 数据库和记忆文件位于 `~/.hexclaw/`：

```bash
# 备份
tar czf hexclaw-backup-$(date +%Y%m%d).tar.gz ~/.hexclaw/

# 恢复
tar xzf hexclaw-backup-20260318.tar.gz -C ~/
```

### 安全审计

定期运行安全审计检查配置安全性：

```bash
hexclaw security audit
```

审计检查项：
- 配置文件权限
- API Key 是否泄露
- 网络暴露风险
- 安全选项是否启用
- 工具权限检查
- 成本预算检查
- 沙箱配置检查

### 升级

```bash
# go install 方式
go install github.com/hexagon-codes/hexclaw/cmd/hexclaw@latest

# 二进制替换
wget https://github.com/hexagon-codes/hexclaw/releases/latest/download/hexclaw-linux-amd64.tar.gz
tar xzf hexclaw-linux-amd64.tar.gz
sudo mv hexclaw /usr/local/bin/
sudo systemctl restart hexclaw

# Docker
docker compose pull
docker compose up -d
```

配置文件向后兼容，通常无需修改即可升级。

---

## 故障排查

### 启动失败

**"没有可用的 LLM Provider"**

至少需要设置一个 LLM API Key：

```bash
export DEEPSEEK_API_KEY="sk-xxx"
```

本地 Ollama 的空 `api_key` 目前只适用于 `POST /api/v1/config/llm/test` 连通性测试；主服务启动仍需要至少一个带 API Key 的 Provider。

**"初始化存储失败"**

检查数据目录权限：

```bash
ls -la ~/.hexclaw/
# 确保当前用户有读写权限
chmod 700 ~/.hexclaw/
```

**端口被占用**

修改监听端口：

```yaml
server:
  port: 7070  # 改为其他端口
```

### 连接问题

**LLM API 超时**

检查网络连接，或配置代理：

```bash
export HTTPS_PROXY="http://proxy:8080"
hexclaw serve
```

对于国内用户，可配置 API 中转：

```yaml
llm:
  providers:
    openai:
      api_key: ${OPENAI_API_KEY}
      base_url: https://your-proxy.com/v1  # API 中转地址
      model: gpt-4o
```

**平台 Webhook 收不到消息**

1. 确认服务器公网可达
2. 检查防火墙是否放行对应端口
3. 使用 ngrok 或 frp 进行内网穿透（开发阶段）：

```bash
ngrok http 16060
# 使用 ngrok 提供的 HTTPS URL 配置 Webhook
```

### 性能问题

**响应慢**

- 启用语义缓存减少重复请求：
  ```yaml
  llm:
    cache:
      enabled: true
      ttl: 24h
  ```
- 使用低成本模型处理简单任务
- 检查知识库大小，适当调整 `chunk_size` 和 `top_k`

**内存占用高**

- 减少上下文保留消息数：
  ```yaml
  compaction:
    enabled: true
    max_messages: 30
    keep_recent: 5
  ```
- 减小语义缓存条目数：
  ```yaml
  llm:
    cache:
      max_entries: 1000
  ```
