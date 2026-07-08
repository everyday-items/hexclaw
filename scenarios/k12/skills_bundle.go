package k12

import "embed"

// bundledSkillsFS 是 K12 场景包**出厂自带**的 Markdown skill（scenarios/k12/skills/*.md，
// 由 scripts/sync-hub-embed.sh 按 hub 的 k12 tag 同步）。go:embed 进二进制 → 首启幂等 seed
// 到 ~/.hexclaw/skills/ → 建档挂载即命中，零下载（产品决策：batteries-included · 本地部署）。
//
//go:embed skills/*.md
var bundledSkillsFS embed.FS

// BundledSkillsFS 返回出厂 seed 的 skill 文件系统，供 composition root 交给 marketplace 首启 seed。
// 返回 fs.FS 语义的 embed.FS；子目录为 "skills"。
func BundledSkillsFS() embed.FS { return bundledSkillsFS }
