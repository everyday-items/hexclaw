# 贡献指南 — hexclaw

hexclaw 是 Hexagon 生态的 **L3 应用**（IM 适配器 / 网关 / Skill / 任务 / 知识库 / API / 桌面端）。

## 分层与复用
- 可依赖 toolkit / ai-core / hexagon；**通用能力一律复用下层，禁止重造**（HTTP/重试/缓存/ID/哈希/HMAC/SSE 等）。
- 应用领域特有（K12 引擎、cron 业务、平台适配、渠道路由）留 hexclaw；通用能力发现后应下沉。

## 本地开发
```bash
go build ./... && go vet ./... && go test -race ./...
golangci-lint run
```
跨仓联调用根目录 go.work（use 四仓）。

## 提交规范
- Conventional Commits；注释中文、只写功能描述，禁暴露内部开发文档/客户名/金额。
- 涉及 DB/缓存/幂等/配额/状态流转的改动，按 bug 修复闭环走 RED→GREEN + 真实环境 E2E。

## PR Checklist
- [ ] build+vet+test -race 全绿、golangci-lint 0 issue
- [ ] 复用下层而非重造；新发现的通用能力评估下沉
- [ ] CHANGELOG.md 记录用户可见变更
