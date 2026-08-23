package main

// shouldPersistRouterConfigSeed 仅在持久化 Agent 为空且配置提供种子时写库。
// 已存在的 Agent 由 SQLite 作为运行态事实源，启动恢复不能用配置快照覆盖它们。
func shouldPersistRouterConfigSeed(loadedFromStore bool, effectiveAgentCount int) bool {
	return !loadedFromStore && effectiveAgentCount > 0
}
