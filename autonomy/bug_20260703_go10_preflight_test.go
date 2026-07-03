package autonomy

// GO-10（BUG-20260703）：estimateCategories 关键词启发式漏判 → AllClear=true 成
// 虚假「无需授权」承诺（运行时 fail-closed 正确，但创建流 UI 被误导为全绿一步）。
// 收口两点：
//  1. media 类别整个不在启发式里（"生成图片/视频"类 prompt 永远漏判）；
//     browser/files 高频同义词（搜索/下载/归档/保存）也漏。多判安全（不产生授权），
//     漏判才是虚假承诺——高信号关键词必须覆盖。
//  2. 结果加 basis 字段（static=显式工具清单静态分析 / heuristic=prompt 启发式），
//     消费方能区分「确定性全绿」与「预估口径全绿」。

import (
	"slices"
	"testing"
)

func TestBug20260703_EstimateCoversMediaAndSynonyms(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"media-图片", "每天早上生成一张风景图片发给我", "media"},
		{"media-视频", "把这段文字做成短视频", "media"},
		{"browser-搜索", "搜索最新的行业新闻并整理摘要", "browser"},
		{"browser-抓取", "抓取官网首页的更新内容", "browser"},
		{"files-下载", "下载周报并存档", "files"},
		{"files-保存", "把结果保存成 markdown", "files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateCategories(PreflightRequest{Source: "cron", Prompt: tc.prompt})
			if !slices.Contains(got, tc.want) {
				t.Errorf("[GO-10] prompt=%q 应预估出 %q，实际 %v（漏判→AllClear 虚假承诺）",
					tc.prompt, tc.want, got)
			}
		})
	}
}

func TestBug20260703_PreflightBasisField(t *testing.T) {
	// prompt 驱动（无静态工具清单）→ heuristic
	res := Preflight(nil, nil, PreflightRequest{Source: "cron", Prompt: "汇总今天的新闻"})
	if res.Basis != "heuristic" {
		t.Errorf("prompt 驱动预检 basis 应为 heuristic，实际 %q", res.Basis)
	}
	// workflow 静态分析（显式工具清单、无 prompt）→ static
	res = Preflight(nil, nil, PreflightRequest{Source: "workflow", Tools: []string{"file_ops"}})
	if res.Basis != "static" {
		t.Errorf("静态工具清单预检 basis 应为 static，实际 %q", res.Basis)
	}
	// 混合（工具清单 + agent 节点 prompt）→ 仍是 heuristic（有预估成分就不承诺确定性）
	res = Preflight(nil, nil, PreflightRequest{Source: "workflow", Tools: []string{"file_ops"}, Prompt: "agent node"})
	if res.Basis != "heuristic" {
		t.Errorf("混合口径预检 basis 应为 heuristic，实际 %q", res.Basis)
	}
}
