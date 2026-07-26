package engineadapter

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// agentReadWriter 内存 Agent 路由（*router.Dispatcher 满足）。
type agentReadWriter interface {
	GetAgent(name string) (*router.AgentConfig, bool)
	UpdateAgent(cfg router.AgentConfig) error
}

type persistedAgentReadWriter interface {
	UpdateAgentPersisted(
		name string,
		update func(router.AgentConfig) (router.AgentConfig, error),
		persist func(*router.AgentConfig) error,
	) error
}

// agentPersister Agent 持久化（router.Store 满足）。可为 nil（仅内存）。
type agentPersister interface {
	SaveAgent(ctx context.Context, agent *router.AgentConfig) error
}

// ProfileAdapter 把孩子档案读写映射到 agents.metadata（经 router）。
type ProfileAdapter struct {
	mu    sync.Mutex
	rw    agentReadWriter
	store agentPersister
}

// NewProfileAdapter 创建 adapter。rw = agentRouter（Dispatcher），store = agentStore（可 nil）。
func NewProfileAdapter(rw agentReadWriter, store agentPersister) *ProfileAdapter {
	return &ProfileAdapter{rw: rw, store: store}
}

var _ usecase.ProfileStore = (*ProfileAdapter)(nil)

// PublishProfile updates only the in-memory router after a profile-bundle
// transaction has already committed agents.metadata in the shared SQLite DB.
func (a *ProfileAdapter) PublishProfile(agentName string, p k12.ChildProfile) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg, ok := a.rw.GetAgent(agentName)
	if !ok {
		return fmt.Errorf("profile: 实例 %q 不存在", agentName)
	}
	updated := *cfg
	updated.Metadata = k12.ApplyProfileToMeta(cfg.Metadata, p)
	return a.rw.UpdateAgent(updated)
}

// GetProfile 从实例 metadata 读孩子档案。
func (a *ProfileAdapter) GetProfile(_ context.Context, agentName string) (k12.ChildProfile, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg, ok := a.rw.GetAgent(agentName)
	if !ok {
		return k12.ChildProfile{}, fmt.Errorf("profile: 实例 %q 不存在", agentName)
	}
	return k12.ProfileFromMeta(cfg.Metadata), nil
}

// SaveProfile 只改 K12 档案键（保留其他 metadata），read-modify-write。
//
// 关键：ApplyProfileToMeta 克隆 map，绝不原地改 router 内部 map。同一 adapter
// 内串行 read-modify-write；先落库再发布内存，内存发布失败则回滚持久化快照。
func (a *ProfileAdapter) SaveProfile(ctx context.Context, agentName string, p k12.ChildProfile) error {
	return a.writeProfile(ctx, agentName, func(meta map[string]string) map[string]string {
		return k12.ApplyProfileToMeta(meta, p)
	})
}

// ReplaceProfile exact-replaces only the K12 metadata namespace. It is used by
// restore; ordinary profile edits retain SaveProfile's non-empty patch semantics.
func (a *ProfileAdapter) ReplaceProfile(ctx context.Context, agentName string, p *k12.ChildProfile) error {
	return a.writeProfile(ctx, agentName, func(meta map[string]string) map[string]string {
		return k12.ReplaceProfileInMeta(meta, p)
	})
}

func (a *ProfileAdapter) writeProfile(ctx context.Context, agentName string, apply func(map[string]string) map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Production Dispatcher supports a single persisted-update critical section.
	// Use it so ordinary profile edits serialize with atomic archive restore and
	// profile readers cannot observe DB/memory skew. Small test fakes retain the
	// legacy Get/Save/Update fallback below.
	if rw, ok := a.rw.(persistedAgentReadWriter); ok {
		return rw.UpdateAgentPersisted(agentName,
			func(current router.AgentConfig) (router.AgentConfig, error) {
				current.Metadata = apply(current.Metadata)
				return current, nil
			},
			func(updated *router.AgentConfig) error {
				if a.store == nil {
					return nil
				}
				if err := a.store.SaveAgent(ctx, updated); err != nil {
					return fmt.Errorf("profile: 持久化档案: %w", err)
				}
				return nil
			},
		)
	}
	cfg, ok := a.rw.GetAgent(agentName)
	if !ok {
		return fmt.Errorf("profile: 实例 %q 不存在", agentName)
	}
	updated := *cfg
	updated.Metadata = apply(cfg.Metadata)
	if a.store != nil {
		if err := a.store.SaveAgent(ctx, &updated); err != nil {
			return fmt.Errorf("profile: 持久化档案: %w", err)
		}
	}
	if err := a.rw.UpdateAgent(updated); err != nil {
		updateErr := fmt.Errorf("profile: 更新内存路由: %w", err)
		if a.store != nil {
			if rollbackErr := a.store.SaveAgent(ctx, cfg); rollbackErr != nil {
				return errors.Join(updateErr, fmt.Errorf("profile: 回滚持久化档案: %w", rollbackErr))
			}
		}
		return updateErr
	}
	return nil
}
