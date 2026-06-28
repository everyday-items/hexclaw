package api

import (
	"reflect"
	"testing"
)

func TestParseParallelRoles(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want []string
	}{
		{"逗号分隔字符串", "researcher, coder, analyst", []string{"researcher", "coder", "analyst"}},
		{"换行分隔", "researcher\ncoder\n", []string{"researcher", "coder"}},
		{"分号分隔", "a;b;c", []string{"a", "b", "c"}},
		{"去重保序", "a, b, a, c, b", []string{"a", "b", "c"}},
		{"去空白项", " a , , b ", []string{"a", "b"}},
		{"[]any 结构化", []any{"researcher", "coder"}, []string{"researcher", "coder"}},
		{"[]string", []string{"x", "y"}, []string{"x", "y"}},
		{"空字符串", "", nil},
		{"nil", nil, nil},
		{"仅分隔符", " , ; \n ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseParallelRoles(c.raw)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseParallelRoles(%v) = %v, 期望 %v", c.raw, got, c.want)
			}
		})
	}
}

// Fuzz：parseParallelRoles 解析任意用户输入不得 panic，且永远返回去重保序结果。
func FuzzParseParallelRoles(f *testing.F) {
	for _, s := range []string{"a,b,c", "", " ; \n ", "x,x,x", "研究员，程序员"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		out := parseParallelRoles(raw)
		seen := map[string]bool{}
		for _, r := range out {
			if r == "" {
				t.Errorf("不得含空项：%q -> %v", raw, out)
			}
			if seen[r] {
				t.Errorf("不得含重复项：%q -> %v", raw, out)
			}
			seen[r] = true
		}
	})
}
