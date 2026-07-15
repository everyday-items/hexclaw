package dingtalk

import (
	"reflect"
	"strings"
	"testing"
)

func TestLiveVisionModelCandidates_ZhipuOnlyUsesSelectedProviderModels(t *testing.T) {
	got := liveVisionModelCandidates(
		"智谱 AI",
		"glm-4v-flash",
		[]string{"glm-4.5", "glm-4v-flash", "glm-4.5"},
		"",
	)
	want := []string{"glm-4v-flash", "glm-4.5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("智谱候选模型必须来自当前路由/智谱配置\ngot:  %v\nwant: %v", got, want)
	}
	for _, model := range got {
		if strings.Contains(model, "/") || strings.HasSuffix(model, ":free") {
			t.Fatalf("智谱候选中混入 OpenRouter 模型 %q", model)
		}
	}
}

func TestLiveVisionModelCandidates_OpenRouterFallbackComesAfterConfiguredModels(t *testing.T) {
	got := liveVisionModelCandidates(
		" OpenRouter ",
		"qwen/qwen3-vl-235b-a22b-instruct",
		[]string{"qwen/qwen3-vl-235b-a22b-instruct", "google/gemma-4-31b-it:free"},
		"",
	)
	want := []string{
		"qwen/qwen3-vl-235b-a22b-instruct",
		"google/gemma-4-31b-it:free",
		"google/gemma-4-26b-a4b-it:free",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("应先尝试路由/已配置模型，最后才用 OpenRouter 专属兜底\ngot:  %v\nwant: %v", got, want)
	}
}

func TestLiveVisionModelCandidates_ExplicitOverrideWinsAndDeduplicates(t *testing.T) {
	got := liveVisionModelCandidates(
		"智谱 AI",
		"glm-4v-flash",
		[]string{"glm-4v-flash", "glm-4.5"},
		" glm-4.5 ",
	)
	want := []string{"glm-4.5", "glm-4v-flash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("显式视觉模型应优先且候选去重\ngot:  %v\nwant: %v", got, want)
	}
}
