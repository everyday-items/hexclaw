package cron

import (
	_ "embed"
	"strings"
)

//go:embed scripts/baidu_hotsearch.star
var baiduHotsearchTemplate string

func deterministicCompiledSpec(prompt string) *JobSpec {
	if !isBaiduHotsearchKnowledgePrompt(prompt) {
		return nil
	}
	return &JobSpec{
		Runtime:    RuntimeStarlark,
		Script:     strings.TrimSpace(baiduHotsearchTemplate),
		TimeoutSec: 60,
	}
}

func isBaiduHotsearchKnowledgePrompt(prompt string) bool {
	p := strings.ToLower(strings.TrimSpace(prompt))
	if p == "" || !strings.Contains(p, "百度热搜") {
		return false
	}
	if !(strings.Contains(p, "知识库") || strings.Contains(p, "knowledge")) {
		return false
	}
	return strings.Contains(p, "写入") ||
		strings.Contains(p, "保存") ||
		strings.Contains(p, "存入") ||
		strings.Contains(p, "入库") ||
		strings.Contains(p, "ingest")
}
