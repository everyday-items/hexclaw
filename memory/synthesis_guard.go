package memory

import (
	"regexp"
	"strings"
)

// 修缺陷D：合成相（画像蒸馏 / 深相整合）原本只查 err/空/==prev 就把 LLM 输出落库——拒答/胡话会覆盖
// 好画像（每轮必注入、无留史）或把好原条折叠成垃圾。本守卫在持久化前**校验输出可用**：非拒答、够长。
//
// 模式针对**拒答短语**（非裸字「无法/不能」，避免误杀「用户不能吃花生」这类正当事实）。保守：宁放过也不误杀。
var refusalPattern = regexp.MustCompile(`(?i)(抱歉[，,]|对不起[，,]|很抱歉|无法根据|无法生成|无法完成|信息不足|没有足够|不便提供|作为(一个)?(AI|人工智能|语言模型)|i'?m sorry|i cannot|i can'?t|unable to (generate|provide|create|complete)|as an ai( language)? model)`)

// isUsableSynthesis 报告合成输出是否可落库（非拒答、达到最小长度）。
func isUsableSynthesis(out string) bool {
	out = strings.TrimSpace(out)
	if len([]rune(out)) < 4 { // 过短（碎片/单字）不可信
		return false
	}
	return !refusalPattern.MatchString(out)
}
