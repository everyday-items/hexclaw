package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// SubjectGroundingWriter 是分科教材写入的可选扩展缝（§4.3：TextbookBinding 按
// Learner × Subject，不用单一全局教材字段）。subject 为六学科中文名；空 = 不分科旧语义。
type SubjectGroundingWriter interface {
	AddGroundingSubject(ctx context.Context, agentName, subject, title, content string) error
}

// SubjectGrounding 是分科教材检索的可选扩展缝：按当前题目学科优先取本学科教材，
// 无本学科教材回退通用（不分科）教材，绝不跨学科串取。subject 空 = 只检索旧版不分科桶。
type SubjectGrounding interface {
	GroundSubject(ctx context.Context, agentName, subject, knowledgePoint, grade string) (text string, found bool, err error)
}

// validateTextbookSubject 校验分科教材学科：空 = 不分科旧语义（合法）；非空必须是六学科之一。
func validateTextbookSubject(subject string) error {
	if subject == "" || k12.ValidTextbookSubject(subject) {
		return nil
	}
	return fmt.Errorf("%w: 学科只允许 %s 或空（不分科），got %q",
		ErrInvalidInput, strings.Join(k12.TextbookSubjects, "/"), subject)
}

// AddGrounding 把家长上传的教材内容按辅导实例隔离入库，供辅导要点召回。
// subject 非空时按学科分科入库（六学科枚举校验）；空保持不分科旧语义（前向兼容）。
// 写侧不支持分科而请求带学科时报错，绝不静默丢学科降级。
func (d Deps) AddGrounding(ctx context.Context, agentName, subject, title, content string) error {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(content) == "" {
		return fmt.Errorf("%w: agent / title / content 必填", ErrInvalidInput)
	}
	if err := validateTextbookSubject(subject); err != nil {
		return err
	}
	if subject != "" {
		sw, ok := d.Grounding.(SubjectGroundingWriter)
		if !ok {
			return fmt.Errorf("usecase: 未配置分科教材写入能力（不降级为不分科，防学科错绑）")
		}
		return sw.AddGroundingSubject(ctx, agentName, subject, title, content)
	}
	w, ok := d.Grounding.(GroundingWriter)
	if !ok {
		return fmt.Errorf("usecase: 未配置教材写入能力")
	}
	return w.AddGrounding(ctx, agentName, title, content)
}
