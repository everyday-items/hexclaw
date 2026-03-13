package engine

import (
	"testing"
)

func TestWorkflowRegistration(t *testing.T) {
	orch := NewOrchestrator(nil, nil)

	wf := &Workflow{
		Name: "test-pipeline",
		Mode: ModePipeline,
		Steps: []Step{
			{AgentRole: "researcher"},
			{AgentRole: "writer"},
		},
	}

	if err := orch.RegisterWorkflow(wf); err != nil {
		t.Fatalf("注册工作流失败: %v", err)
	}

	got, ok := orch.GetWorkflow("test-pipeline")
	if !ok {
		t.Fatal("工作流未找到")
	}
	if got.Name != "test-pipeline" {
		t.Errorf("名称不匹配: %s", got.Name)
	}
	if len(got.Steps) != 2 {
		t.Errorf("步骤数不匹配: %d", len(got.Steps))
	}

	list := orch.ListWorkflows()
	if len(list) != 1 {
		t.Errorf("工作流数量不匹配: %d", len(list))
	}

	orch.RemoveWorkflow("test-pipeline")
	_, ok = orch.GetWorkflow("test-pipeline")
	if ok {
		t.Error("工作流应已被移除")
	}
}

func TestWorkflowValidation(t *testing.T) {
	orch := NewOrchestrator(nil, nil)

	// 空名称
	err := orch.RegisterWorkflow(&Workflow{Steps: []Step{{AgentRole: "a"}}})
	if err == nil {
		t.Error("空名称应报错")
	}

	// 空步骤
	err = orch.RegisterWorkflow(&Workflow{Name: "empty"})
	if err == nil {
		t.Error("空步骤应报错")
	}
}
