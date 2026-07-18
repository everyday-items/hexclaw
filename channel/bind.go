package channel

import "fmt"

// Binding 描述一个私聊目标与辅导实例（TutorAgent）的绑定。
type Binding struct {
	Platform   string
	InstanceID string
	ChatID     string
	AgentName  string
}

// CheckExclusiveBind 限绑校验（架构设计-v0.5.0 §3.12，2026-07-18 裁决；语义归属通道层，
// 渠道中立）：同一私聊目标同一时间只能绑定一个 TutorAgent——妈妈的同一个钉钉号不能
// 同时是两个孩子助手的接收人，否则入站照片归属无解（卷面号仅 Learner 内唯一救不了
// 绑定歧义）。家庭域唯一卷号 + learner 短码/校验位列为 v0.6 演进。
//
// 返回值：
//   - already=true：同实例重复绑定（幂等，调用方直接成功返回、不重写规则）；
//   - err != nil：该私聊目标已绑其他实例 → 拒绝并明示原因、引导先解绑（文案家长向）。
func CheckExclusiveBind(existing []Binding, candidate Binding) (already bool, err error) {
	for _, b := range existing {
		if b.Platform != candidate.Platform || b.InstanceID != candidate.InstanceID || b.ChatID != candidate.ChatID {
			continue
		}
		if b.AgentName == candidate.AgentName {
			return true, nil // 幂等：同一实例重复绑定不报错、不重写
		}
		return false, fmt.Errorf("这个私聊已经在接收「%s」的消息：一个私聊同时只能接收一个孩子的助手，请先解绑「%s」再绑定「%s」",
			b.AgentName, b.AgentName, candidate.AgentName)
	}
	return false, nil
}
