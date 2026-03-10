package agents

import (
	"github.com/everyday-items/hexagon"
)

// CreateResearchTeam 创建研究团队
//
// 由研究员和写作者组成，顺序执行：
//  1. 研究员负责调研和分析
//  2. 写作者将研究结果整理成报告
//
// 适用场景：深度研究报告、竞品分析、技术调研
func CreateResearchTeam(provider hexagon.Provider) *hexagon.Team {
	researcher := hexagon.NewReActAgent(
		hexagon.AgentWithName("researcher"),
		hexagon.AgentWithLLM(provider),
		hexagon.AgentWithRole(ResearcherRole),
		hexagon.AgentWithMaxIterations(15),
	)

	writer := hexagon.NewReActAgent(
		hexagon.AgentWithName("writer"),
		hexagon.AgentWithLLM(provider),
		hexagon.AgentWithRole(WriterRole),
		hexagon.AgentWithMaxIterations(10),
	)

	return hexagon.NewTeam("research-team",
		hexagon.WithAgents(researcher, writer),
		hexagon.WithMode(hexagon.TeamModeSequential),
		hexagon.WithMaxRounds(2),
		hexagon.WithTeamDescription("研究团队：研究员调研 → 写作者整理报告"),
	)
}

// CreateDevTeam 创建开发团队
//
// 由分析师和编码者组成，协作执行：
//   - 分析师负责需求分析和方案设计
//   - 编码者负责代码实现和审查
//
// 适用场景：代码生成、技术方案设计、代码审查
func CreateDevTeam(provider hexagon.Provider) *hexagon.Team {
	analyst := hexagon.NewReActAgent(
		hexagon.AgentWithName("analyst"),
		hexagon.AgentWithLLM(provider),
		hexagon.AgentWithRole(AnalystRole),
		hexagon.AgentWithMaxIterations(10),
	)

	coder := hexagon.NewReActAgent(
		hexagon.AgentWithName("coder"),
		hexagon.AgentWithLLM(provider),
		hexagon.AgentWithRole(CoderRole),
		hexagon.AgentWithMaxIterations(15),
	)

	return hexagon.NewTeam("dev-team",
		hexagon.WithAgents(analyst, coder),
		hexagon.WithMode(hexagon.TeamModeSequential),
		hexagon.WithMaxRounds(2),
		hexagon.WithTeamDescription("开发团队：分析师设计 → 编码者实现"),
	)
}

// CreateTranslateTeam 创建翻译团队
//
// 由翻译专家和写作者组成，顺序执行：
//  1. 翻译专家进行初始翻译
//  2. 写作者润色和本地化
//
// 适用场景：高质量翻译、文档本地化
func CreateTranslateTeam(provider hexagon.Provider) *hexagon.Team {
	translator := hexagon.NewReActAgent(
		hexagon.AgentWithName("translator"),
		hexagon.AgentWithLLM(provider),
		hexagon.AgentWithRole(TranslatorRole),
		hexagon.AgentWithMaxIterations(5),
	)

	writer := hexagon.NewReActAgent(
		hexagon.AgentWithName("writer"),
		hexagon.AgentWithLLM(provider),
		hexagon.AgentWithRole(WriterRole),
		hexagon.AgentWithMaxIterations(5),
	)

	return hexagon.NewTeam("translate-team",
		hexagon.WithAgents(translator, writer),
		hexagon.WithMode(hexagon.TeamModeSequential),
		hexagon.WithMaxRounds(2),
		hexagon.WithTeamDescription("翻译团队：翻译专家翻译 → 写作者润色"),
	)
}
