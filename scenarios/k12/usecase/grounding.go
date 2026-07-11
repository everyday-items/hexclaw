package usecase

import (
	"context"
	"fmt"
	"strings"
)

// AddGrounding 把家长上传的教材内容按辅导实例隔离入库，供备课卡召回。
func (d Deps) AddGrounding(ctx context.Context, agentName, title, content string) error {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(content) == "" {
		return fmt.Errorf("%w: agent / title / content 必填", ErrInvalidInput)
	}
	w, ok := d.Grounding.(GroundingWriter)
	if !ok {
		return fmt.Errorf("usecase: 未配置教材写入能力")
	}
	return w.AddGrounding(ctx, agentName, title, content)
}
