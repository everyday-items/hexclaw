package cron

import (
	"reflect"
	"testing"
)

// TestJob_PromptFieldRemoved 断言 Job 模型完成 v2 升级：
//   - 删除遗留的 Prompt 字段（破坏性升级，禁止保留兼容）
//   - 新增 Spec *JobSpec（编译产物，运行时直接执行）
//   - 新增 SourcePrompt string（用户原始 prompt，仅用于 UI 展示与重编译来源）
func TestJob_PromptFieldRemoved(t *testing.T) {
	jt := reflect.TypeOf(Job{})

	if _, has := jt.FieldByName("Prompt"); has {
		t.Fatal("Job.Prompt 字段必须移除（破坏性升级，参考 .claude/cron-script-compilation-design.md §3.1）")
	}

	specField, has := jt.FieldByName("Spec")
	if !has {
		t.Fatal("Job.Spec 字段缺失")
	}
	if specField.Type.Kind() != reflect.Pointer {
		t.Fatalf("Job.Spec 必须是 *JobSpec 指针，实际 %s", specField.Type.Kind())
	}
	if specField.Type.Elem().Name() != "JobSpec" {
		t.Fatalf("Job.Spec 必须指向 JobSpec，实际 %s", specField.Type.Elem().Name())
	}

	srcField, has := jt.FieldByName("SourcePrompt")
	if !has {
		t.Fatal("Job.SourcePrompt 字段缺失")
	}
	if srcField.Type.Kind() != reflect.String {
		t.Fatalf("Job.SourcePrompt 必须是 string，实际 %s", srcField.Type.Kind())
	}
}

// TestJobSpec_Shape 锁定 JobSpec / CompileMeta 的字段契约，避免重构走样。
func TestJobSpec_Shape(t *testing.T) {
	st := reflect.TypeOf(JobSpec{})
	expected := map[string]reflect.Kind{
		"Runtime":    reflect.String,
		"Script":     reflect.String,
		"Deps":       reflect.Slice,
		"Inputs":     reflect.Map,
		"TimeoutSec": reflect.Int,
		"Compiled":   reflect.Struct,
	}
	for name, kind := range expected {
		f, has := st.FieldByName(name)
		if !has {
			t.Errorf("JobSpec.%s 缺失", name)
			continue
		}
		if f.Type.Kind() != kind {
			t.Errorf("JobSpec.%s kind 期望 %s 实际 %s", name, kind, f.Type.Kind())
		}
	}

	mt := reflect.TypeOf(CompileMeta{})
	for _, name := range []string{"Model", "At", "TokensIn", "TokensOut", "Hash"} {
		if _, has := mt.FieldByName(name); !has {
			t.Errorf("CompileMeta.%s 缺失", name)
		}
	}
}
