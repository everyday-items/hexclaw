package adapter

import (
	"fmt"
	"math"
	"strconv"
)

// 请求级采样参数使用独立 metadata 键，避免 Agent 路由阶段写入的默认值覆盖
// REST/WebSocket 入口上用户对本次请求的显式选择。
const (
	MetadataRequestTemperature = "request_temperature"
	MetadataRequestMaxTokens   = "request_max_tokens"
	// MetadataAgent* are trusted router-to-engine keys. External chat ingress
	// must always remove them before the router stamps the selected Agent.
	MetadataAgentTemperature = "agent_temperature"
	MetadataAgentMaxTokens   = "agent_max_tokens"
	// MaxSamplingTokens is a defense-in-depth ceiling for one completion. It is
	// deliberately above current model output limits while preventing arbitrary
	// metadata from requesting unbounded work/cost.
	MaxSamplingTokens = 1_000_000
)

// ApplyRequestSamplingOverrides 校验并写入请求级采样参数。
//
// metadata 来自不可信客户端，因此先移除可伪造的内部键；只有结构化字段经过
// 校验后才能重新写入。nil 表示本次请求未显式设置，temperature=0 保留为有效值。
func ApplyRequestSamplingOverrides(metadata map[string]string, temperature *float64, maxTokens *int) error {
	if metadata == nil {
		return fmt.Errorf("metadata 不能为空")
	}
	delete(metadata, MetadataRequestTemperature)
	delete(metadata, MetadataRequestMaxTokens)
	delete(metadata, MetadataAgentTemperature)
	delete(metadata, MetadataAgentMaxTokens)

	if temperature != nil {
		if math.IsNaN(*temperature) || math.IsInf(*temperature, 0) || *temperature < 0 || *temperature > 2 {
			return fmt.Errorf("temperature 必须在 [0,2] 区间")
		}
		metadata[MetadataRequestTemperature] = strconv.FormatFloat(*temperature, 'g', -1, 64)
	}
	if maxTokens != nil {
		if *maxTokens <= 0 || *maxTokens > MaxSamplingTokens {
			return fmt.Errorf("max_tokens 必须是 [1,%d] 区间内的整数", MaxSamplingTokens)
		}
		metadata[MetadataRequestMaxTokens] = strconv.Itoa(*maxTokens)
	}
	return nil
}
