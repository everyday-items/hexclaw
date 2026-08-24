package main

import (
	"os"
	"strings"
	"testing"
)

// 生产装配必须显式提供服务端积累元数据派生器，不能依赖测试 fake 或 usecase fallback。
func TestProductionK12CompositionInjectsAccumulationMetadataDeriver(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"k12engineadapter.NewAccumulationMetadataAdapter(accumulationMetadataGenFn)",
		"k12assembly.WithAccumulationMetadataDeriver(",
	} {
		if got := strings.Count(string(source), required); got != 1 {
			t.Fatalf("production accumulation metadata wiring %q count=%d, want exactly 1", required, got)
		}
	}
}
