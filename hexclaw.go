// Package hexclaw 提供企业级安全的个人 AI Agent
//
// HexClaw（河蟹）是一个安全、开源、可自托管的个人 AI Agent，
// 支持多平台接入（飞书/钉钉/微信/Telegram/Discord/Slack/Web），
// 六层安全网关、LLM 智能路由、Skill 沙箱执行等企业级能力。
//
// 快速开始:
//
//	export DEEPSEEK_API_KEY="sk-xxx"
//	hexclaw serve
//
// 架构概览:
//
//	cmd/hexclaw/     入口
//	adapter/         平台适配器 (8 个)
//	engine/          ReAct 引擎
//	gateway/         六层安全网关
//	api/             HTTP/WebSocket API
//	config/          配置管理
//	skill/           Skill 系统
//	knowledge/       知识库/RAG
//	llmrouter/       LLM 智能路由
//	session/         会话管理
//	memory/          文件记忆
//	storage/         持久化存储
package hexclaw

// Version 当前版本号，通过 -ldflags 在构建时注入
const Version = "0.1.0"
