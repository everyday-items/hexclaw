// manifest.go K12 场景的 ScenarioManifest v2 声明面（架构设计 §6.2；ADR-K12-002）。
//
// 冻结范围（文档顶部冻结清单 / K12-INV-014）在此钉死：初中、高中 unavailable；
// 物理、化学、音乐不出现。Skill catalog 从内嵌 skills 派生、数据对象/动作从 Pack 派生
// （§6.2 禁止手写副本）；K12-INV-013：不硬编码任何模型名。
package k12

import (
	"io/fs"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenario"
)

// Manifest v2 头部常量。
const (
	ScenarioID      = "k12"
	ScenarioVersion = "0.5.0"
	// ManifestContractVersion Manifest 契约版本：v2（§6.2 唯一版本化 Manifest）。
	ManifestContractVersion = 2
	// MountPath HTTP 挂载前缀（多场景路由命名空间隔离；composition root 以此 Mount）。
	MountPath = "/api/k12"
)

// Manifest 返回 K12 的 ScenarioManifest v2。Pack 原样作为六缝资源载荷内嵌
// （向后兼容演进：六缝契约不变，Manifest 是其上的版本化声明壳）。
func Manifest(constraint scenario.ConstraintProvider) *scenario.Manifest {
	return &scenario.Manifest{
		ID:                 ScenarioID,
		Version:            ScenarioVersion,
		ContractVersion:    ManifestContractVersion,
		MinContractVersion: ManifestContractVersion,
		MountPath:          MountPath,
		// 阶段能力（§1.3 学段表；冻结：初中/高中必须 unavailable）。
		Stages: map[string]scenario.Availability{
			"小学": scenario.Available,
			"初中": scenario.Unavailable,
			"高中": scenario.Unavailable,
		},
		// 当前小学学科能力（§1.3 学科表）；物理/化学/音乐按冻结清单不声明。
		Subjects: map[string]scenario.Availability{
			"数学":   scenario.Available,
			"语文":   scenario.Available,
			"英语":   scenario.Available,
			"科学":   scenario.Available,
			"信息科技": scenario.Available,
			"美术":   scenario.Available,
		},
		// Skill catalog 从出厂内嵌 skills 派生（完整目录与验收以 K12-Skill清单-v0.5.0.md 为准）。
		Skills: bundledSkillDecls(),
		// Tool capability：平台工具面注册的 K12 工具（composition root skills.Register）。
		Tools: []string{"k12_grade", "k12_review"},
		// 已达质量门的确定性验证器学科（§4.7：数学/语文/英语达门；科学/信息科技过门前
		// 不声明为可用验证器——宁可窄而真）。
		Validators: []string{"数学", "语文", "英语"},
		// 渠道投影能力（§6.10）：钉钉是 v0.5.0 唯一真实通道；飞书/企微为诚实 stub，不声明。
		ChannelProjections: []string{"dingtalk"},
		// 数据迁移标识（storage/migrate 版本；§6.9 类型化存储 + 安装台账）。
		Migrations: []string{"v8-agent-records", "v9-k12-typed-store", "v10-scenario-installations", "v11-k12-dedupe-release-backfill"},
		// 定时工作流描述符（§6.11/§3.13：安装只登记能力；任务实例按 Learner 建档 provision）。
		// 与 usecase.DefaultCronSpecs 的 kind 集合由契约测试钉一致（Manifest 为唯一事实源）。
		ScheduledWorkflows: []string{"weekly-sheet", "return-reminder", "semester-spring", "semester-fall"},
		Resources:          Pack(constraint),
	}
}

// bundledSkillDecls 从内嵌 skills 目录派生 Skill catalog（避免手写副本；
// 版本随 hub tag 同步在文件内 frontmatter，此处仅登记名称）。
func bundledSkillDecls() []scenario.SkillDecl {
	entries, err := fs.ReadDir(bundledSkillsFS, "skills")
	if err != nil {
		return nil
	}
	decls := make([]scenario.SkillDecl, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		decls = append(decls, scenario.SkillDecl{Name: strings.TrimSuffix(name, ".md")})
	}
	sort.Slice(decls, func(i, j int) bool { return decls[i].Name < decls[j].Name })
	return decls
}
