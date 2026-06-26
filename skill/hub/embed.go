package hub

import _ "embed"

// 内嵌的市场快照：随二进制出厂，作为「离线优先」catalog 的终极兜底层。
//
// 设计理由：registry 体量 < 100KB、纯静态、且版本随发布分支钉死（与 DefaultHubBranch 一致），
// 因此内嵌它「又便宜又永远契约一致」——即便全新机器首次离线、GitHub 不可达或被限流，
// 市场也永不为空，且内容与本二进制 100% 匹配。
//
// 发版例行：每次发布时从 pinned 的 hexclaw-hub 分支同步这两个文件
//   cp <hexclaw-hub>/index.json        skill/hub/embed/index.json
//   cp <hexclaw-hub>/mcp-registry.json skill/hub/embed/mcp-registry.json
// （见 scripts/sync-hub-embed.sh）。

//go:embed embed/index.json
var embeddedCatalogJSON []byte

//go:embed embed/mcp-registry.json
var embeddedMcpRegistryJSON []byte
