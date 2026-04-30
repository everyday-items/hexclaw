package events

import (
	"context"
	"errors"

	"github.com/hexagon-codes/hexclaw/featureflag"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// FlagEventsTransportV1 控制 H6 事件协议是否启用。flag 关闭时所有 Emit 是 no-op。
const FlagEventsTransportV1 = "events.transport.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagEventsTransportV1,
		Default:      true, // alpha 强制 OFF
		Description:  "Enable structured event protocol with remote transport (events.Emit + Sinks).",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// Emitter 是事件发射器，持有 Sink + 默认 Source 标签。
type Emitter struct {
	sink          Sink
	defaultSource string
}

// NewEmitter 构造一个 Emitter。sink 为 nil 时退化为 NoopSink。
func NewEmitter(sink Sink, defaultSource string) *Emitter {
	if sink == nil {
		sink = NewNoopSink()
	}
	return &Emitter{sink: sink, defaultSource: defaultSource}
}

// Sink 返回 emitter 持有的底层 sink（用于关闭 / 单测断言）。
func (em *Emitter) Sink() Sink { return em.sink }

// Emit 投递一条事件：
//   - 校验 Type 非空（缺 Type 直接返回 error，不进 Sink）
//   - 自动补 Source（如果调用方没设）
//   - Sink 失败仅记 fallback log，不上抛——投递必须 best-effort
//
// 如果 emitter 是 nil，这是 no-op。
func (em *Emitter) Emit(ctx context.Context, e Event) error {
	if em == nil {
		return nil
	}
	if !e.valid() {
		return errors.New("events: empty Type")
	}
	if e.Source == "" {
		e.Source = em.defaultSource
	}
	if err := em.sink.Submit(ctx, e); err != nil {
		// fallback：不让事件丢失（至少有 logger 一份）
		logger.Warn("[events] sink submit failed", "type", e.Type, "err", err)
		return err
	}
	return nil
}

// Close 关闭底层 sink。
func (em *Emitter) Close() error {
	if em == nil || em.sink == nil {
		return nil
	}
	return em.sink.Close()
}

// Emit 是包级便捷函数：从 ctx 取出 Emitter（WithEmitter 注入），并查询 feature flag。
//
// flag OFF 或 ctx 中无 emitter 时直接返回 nil。
func Emit(ctx context.Context, e Event) error {
	if !featureflag.Enabled(ctx, FlagEventsTransportV1) {
		return nil
	}
	em := emitterFromCtx(ctx)
	if em == nil {
		return nil
	}
	return em.Emit(ctx, e)
}
