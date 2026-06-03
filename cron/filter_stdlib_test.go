package cron

import (
	"reflect"
	"testing"
)

// TestFilterStdlibDeps 锁定 LLM 误填 stdlib 的过滤行为。
//
// 边界覆盖：
//   - 纯 stdlib 名 → 全过滤
//   - PyPI 包 → 保留
//   - 带 version 约束（json==2.0、requests>=2.30）→ 按 base name 判断
//   - 含空白 / 大小写边界（按设计仅严格小写匹配，"JSON" 视为非 stdlib 保留）
func TestFilterStdlibDeps(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"all stdlib", []string{"json", "os", "sys", "re", "time", "datetime"}, []string{}},
		{"all third party", []string{"requests", "httpx", "beautifulsoup4"}, []string{"requests", "httpx", "beautifulsoup4"}},
		{"mixed", []string{"json", "requests", "os", "lxml"}, []string{"requests", "lxml"}},
		{"version pinned stdlib", []string{"json==2.0", "requests>=2.30"}, []string{"requests>=2.30"}},
		{"empty / whitespace", []string{"", "  ", "json"}, []string{}},
		{"version range third party", []string{"requests>=2.0,<3.0"}, []string{"requests>=2.0,<3.0"}},
		{"case sensitive (Json 视为非 stdlib)", []string{"JSON", "Os"}, []string{"JSON", "Os"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterStdlibDeps(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFilterStdlibDeps_Idempotent 不变量：过滤是幂等的。
func TestFilterStdlibDeps_Idempotent(t *testing.T) {
	cases := [][]string{
		{"json", "requests"},
		{"os", "sys"},
		{"httpx>=0.27"},
		nil,
	}
	for _, c := range cases {
		once := filterStdlibDeps(c)
		twice := filterStdlibDeps(once)
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("非幂等: in=%v once=%v twice=%v", c, once, twice)
		}
	}
}
