package engine

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/session"
	"github.com/hexagon-codes/hexclaw/storage"
)

// SuggestSessionTitle 基于会话消息生成更自然的标题。
//
// 它走当前生效的 LLM 路由，但关闭 thinking，避免标题生成拖慢主链路。
func (e *ReActEngine) SuggestSessionTitle(ctx context.Context, messages []*storage.MessageRecord) (string, error) {
	if len(messages) == 0 {
		return "", nil
	}

	provider, providerName, err := e.resolveProvider(ctx, "", nil)
	if err != nil {
		return "", err
	}

	modelName := e.getProviderModel(providerName, map[string]string{"thinking": "off"})
	if modelName != "" {
		provider = wrapModelOverrideProvider(provider, modelName)
	}

	title, err := session.SuggestTitle(ctx, provider, messages)
	if err != nil {
		return "", fmt.Errorf("生成会话标题失败: %w", err)
	}
	return title, nil
}
