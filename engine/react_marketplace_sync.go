package engine

import (
	"fmt"
	"log"

	"github.com/hexagon-codes/hexclaw/skill/marketplace"
)

// SeedMarketplaceSkillNames 在启动时记录当前磁盘上的 Markdown 技能名，
// 供后续 SyncMarkdownSkillsFromMarketplace 正确卸载已从目录删除的技能。
func (e *ReActEngine) SeedMarketplaceSkillNames(names []string) {
	if len(names) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mpTracked == nil {
		e.mpTracked = make(map[string]struct{})
	}
	for _, n := range names {
		if n != "" {
			e.mpTracked[n] = struct{}{}
		}
	}
}

// SyncMarkdownSkillsFromMarketplace 将磁盘技能目录与运行时注册表对齐：
// 新增文件 → Register；已删除 → Unregister；并同步启用状态。
func (e *ReActEngine) SyncMarkdownSkillsFromMarketplace(mp *marketplace.Marketplace) error {
	if mp == nil || e.skills == nil {
		return nil
	}
	if err := mp.Init(); err != nil {
		return fmt.Errorf("刷新技能目录失败: %w", err)
	}
	list := mp.List()
	want := make(map[string]struct{}, len(list))
	for _, md := range list {
		n := md.Meta.Name
		if n == "" {
			continue
		}
		want[n] = struct{}{}
	}

	e.mu.Lock()
	tracked := e.mpTracked
	if tracked == nil {
		tracked = make(map[string]struct{})
	}
	// 复制一份待删除列表，避免在迭代中修改 map
	var toRemove []string
	for name := range tracked {
		if _, ok := want[name]; !ok {
			toRemove = append(toRemove, name)
		}
	}
	e.mu.Unlock()

	sk := e.skills
	for _, name := range toRemove {
		if err := sk.Unregister(name); err != nil {
			log.Printf("技能市场同步: 注销 %q 跳过: %v", name, err)
		}
	}

	for _, md := range list {
		n := md.Meta.Name
		if n == "" {
			continue
		}
		if _, ok := sk.Get(n); !ok {
			w := marketplace.WrapAsSkill(md)
			if err := sk.Register(w); err != nil {
				log.Printf("技能市场同步: 注册 %q 失败: %v", n, err)
				continue
			}
		}
		if err := sk.SetEnabled(n, mp.IsEnabled(n)); err != nil {
			log.Printf("技能市场同步: 设置 %q 启用状态失败: %v", n, err)
		}
	}

	e.mu.Lock()
	e.mpTracked = want
	e.mu.Unlock()
	return nil
}
