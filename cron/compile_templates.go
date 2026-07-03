package cron

import (
	_ "embed"
	"strings"
)

//go:embed scripts/baidu_hotsearch.star
var baiduHotsearchTemplate string

//go:embed scripts/baidu_hotsearch_collect.star
var baiduHotsearchCollectTemplate string

// deterministicCompiledSpec short-circuits the LLM for prompts whose data
// source and extraction path are already known-stable. An LLM cannot know a
// page's real embedded structure, so it fabricates parse paths that fail at
// runtime with unfixable errors (BUG-20260704: "no items found in data
// structure" — self-heal recompiles kept guessing and never converged). For
// known sources the archived, live-verified template is strictly better:
// zero tokens, deterministic, correct.
func deterministicCompiledSpec(prompt string) *JobSpec {
	p := strings.ToLower(strings.TrimSpace(prompt))
	if p == "" || !strings.Contains(p, "百度热搜") {
		return nil
	}
	// KB intent first: it is the more specific ask (collect AND persist).
	if hasKnowledgeIngestIntent(p) {
		return &JobSpec{
			Runtime:    RuntimeStarlark,
			Script:     strings.TrimSpace(baiduHotsearchTemplate),
			TimeoutSec: 60,
		}
	}
	// Collect-only intent (BUG-20260704: "百度热搜 TOP20 采集" previously missed
	// the gate and fell into LLM guessing): same extraction, no silent KB write.
	if hasCollectionIntent(p) {
		return &JobSpec{
			Runtime:    RuntimeStarlark,
			Script:     strings.TrimSpace(baiduHotsearchCollectTemplate),
			TimeoutSec: 60,
		}
	}
	return nil
}

// hasKnowledgeIngestIntent reports a prompt that asks to persist the hot
// search into the knowledge base (the original, narrower gate).
func hasKnowledgeIngestIntent(p string) bool {
	if !(strings.Contains(p, "知识库") || strings.Contains(p, "knowledge")) {
		return false
	}
	return strings.Contains(p, "写入") ||
		strings.Contains(p, "保存") ||
		strings.Contains(p, "存入") ||
		strings.Contains(p, "入库") ||
		strings.Contains(p, "ingest")
}

// hasCollectionIntent reports a mechanical collect/fetch ask. Kept to concrete
// collection verbs and list-shaped nouns so cognitive prompts ("百度热搜是什么")
// still go through normal classification/compilation.
func hasCollectionIntent(p string) bool {
	for _, kw := range []string{
		"采集", "收集", "抓取", "爬取", "获取", "拉取",
		"top", "前10", "前20", "前 10", "前 20", "榜单", "热搜榜",
	} {
		if strings.Contains(p, kw) {
			return true
		}
	}
	return false
}
