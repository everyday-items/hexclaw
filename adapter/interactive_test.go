package adapter

import "testing"

func TestInteractivePayload_IsValid(t *testing.T) {
	cases := []struct {
		name    string
		payload *InteractivePayload
		want    bool
	}{
		{"nil", nil, false},
		{"buttons valid", &InteractivePayload{
			Type:    InteractiveTypeButtons,
			Buttons: []InteractiveButton{{Label: "是", Action: "yes"}},
		}, true},
		{"buttons empty", &InteractivePayload{Type: InteractiveTypeButtons}, false},
		{"select valid", &InteractivePayload{
			Type:    InteractiveTypeSelect,
			Options: []InteractiveOption{{Label: "1", Value: "1"}},
		}, true},
		{"select empty", &InteractivePayload{Type: InteractiveTypeSelect}, false},
		{"approval valid", &InteractivePayload{
			Type:     InteractiveTypeApproval,
			Approval: &InteractiveApproval{Subject: "删除 12 道错题"},
		}, true},
		{"approval missing subject", &InteractivePayload{
			Type:     InteractiveTypeApproval,
			Approval: &InteractiveApproval{},
		}, false},
		{"approval nil", &InteractivePayload{Type: InteractiveTypeApproval}, false},
		{"card valid", &InteractivePayload{
			Type: InteractiveTypeCard,
			Card: &InteractiveCard{Title: "学情周报"},
		}, true},
		{"card missing title", &InteractivePayload{
			Type: InteractiveTypeCard,
			Card: &InteractiveCard{},
		}, false},
		{"unknown type", &InteractivePayload{Type: "unknown"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.payload.IsValid(); got != c.want {
				t.Errorf("IsValid()=%v want=%v", got, c.want)
			}
		})
	}
}

func TestInteractivePayload_ResolvedFlow(t *testing.T) {
	p := &InteractivePayload{
		Type:   InteractiveTypeButtons,
		Prompt: "是这道题吗？",
		Buttons: []InteractiveButton{
			{Label: "是", Action: "confirm", Variant: ButtonPrimary},
			{Label: "不是", Action: "reject"},
		},
	}
	if !p.IsValid() {
		t.Fatal("初始 payload 应有效")
	}
	// 模拟用户点击
	p.Resolved = &InteractiveResolved{Action: "confirm", Label: "是"}
	if !p.IsValid() {
		t.Fatal("resolved 后应仍然有效")
	}
}
