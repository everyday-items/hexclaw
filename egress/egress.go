// Package egress 把「分级隐私」落成代码边界而非文档声明（架构 §8.2 · 评审 P0-5）。
//
// 所有出网请求（LLM / vision / embed）必须携带 {Purpose, DataClass}，由 Policy 判定
// 是否允许上云 + 产出审计条目。核心红线：敏感数据类（个人档案 / 业务记录 / 记忆画像）
// 只允许在白名单用途（图像识别 / 受验证推理）下出本机；其余一律拦截。
//
// 平台级、领域中性——它只认识"数据敏感类 × 用途"，不认识任何业务场景；
// 具体业务数据到敏感类的映射由调用点（场景包）决定。
package egress

import "fmt"

// Purpose 出网用途。
type Purpose string

const (
	PurposeVisionOCR   Purpose = "vision_ocr"   // 图像/OCR 识别
	PurposeSolveVerify Purpose = "solve_verify" // 受验证的推理（需程序/工具验证的推理）
	PurposeGeneralChat Purpose = "general_chat" // 通用对话
	PurposeRAGEmbed    Purpose = "rag_embed"    // 文档向量化
)

// DataClass 数据敏感类别（通用分级；业务概念到此的映射在调用点）。
type DataClass string

const (
	ClassSensitiveProfile DataClass = "sensitive_profile" // 敏感个人档案（默认不出网）
	ClassSensitiveMedia   DataClass = "sensitive_media"   // 敏感媒体（图片等，识别用途需出网）
	ClassRecord           DataClass = "record"            // 业务记录（默认不出网）
	ClassMemory           DataClass = "memory"            // 记忆画像（默认不出网）
	ClassDocument         DataClass = "document"          // 文档（可向量化）
	ClassGeneral          DataClass = "general"           // 无敏感数据的一般内容
)

// Request 一次出网请求的元数据。
type Request struct {
	Purpose   Purpose
	DataClass DataClass
	AuditID   string // 关联审计条目（可空，分期留痕）
}

// Decision 判定结果。
type Decision struct {
	AllowCloud bool
	Reason     string
}

// sensitiveClasses 默认不得出本机的数据类。
var sensitiveClasses = map[DataClass]bool{
	ClassSensitiveProfile: true,
	ClassRecord:           true,
	ClassMemory:           true,
}

// purposeAllowsSensitive 白名单：允许携带敏感数据出网的用途 → 该用途允许的敏感类。
var purposeAllowsSensitive = map[Purpose]map[DataClass]bool{
	// 图像识别：敏感媒体（含手写/姓名的图片）必须出网给云端 vision。
	PurposeVisionOCR: {ClassSensitiveMedia: true},
	// 受验证推理：文本出网给云端强模型；敏感个人档案随请求注入允许。
	PurposeSolveVerify: {ClassSensitiveProfile: true, ClassSensitiveMedia: true},
}

// Policy 出口策略（网关持有；enforcement 强制，audit 可分期）。
type Policy struct {
	// OnAudit 每次判定回调（留痕/监控）。可为 nil。
	OnAudit func(req Request, d Decision)
}

// Evaluate 判定一次出网请求是否允许上云。
//
// 规则：
//   - 非敏感类（document/general/homework_image）：允许（document/general 上云无隐私风险）；
//   - 敏感类（child_profile/record/memory）：仅当用途在白名单且该用途显式允许此类时放行，
//     否则拦截——这条就是"只有白名单用途出本机"的代码化。
func (p *Policy) Evaluate(req Request) Decision {
	d := p.decide(req)
	if p.OnAudit != nil {
		p.OnAudit(req, d)
	}
	return d
}

func (p *Policy) decide(req Request) Decision {
	if !sensitiveClasses[req.DataClass] {
		return Decision{AllowCloud: true, Reason: "非敏感数据类"}
	}
	if allowed, ok := purposeAllowsSensitive[req.Purpose]; ok && allowed[req.DataClass] {
		return Decision{AllowCloud: true, Reason: fmt.Sprintf("用途 %s 白名单允许 %s 出网", req.Purpose, req.DataClass)}
	}
	return Decision{
		AllowCloud: false,
		Reason:     fmt.Sprintf("敏感数据类 %s 不允许在用途 %s 下出本机", req.DataClass, req.Purpose),
	}
}

// Guard 便捷断言：若不允许上云则返回 error（供出网调用点前置守卫）。
func (p *Policy) Guard(req Request) error {
	if d := p.Evaluate(req); !d.AllowCloud {
		return fmt.Errorf("egress 拦截: %s", d.Reason)
	}
	return nil
}
