package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 冻结#2 / 执行计划 §5.4 零容忍第二条：「初中、高中可选择或可绕过进入」。
// 建档主门（UpdateProfile 12 档白名单）之外还有两条绕过通道，本文件逐条钉死：
//   通道一：冷启动推断/落库（InferProfile / ColdStartProvision）
//   通道二：备份恢复（Restore 一份含初中档案的 .hexbak 即绕过建档守门）
// 2026-07-18 提级裁决：不以"遗留点"入账，与 UpdateProfile 同级为发布阻断守卫。

// TestFrozenBypass_ColdStartRejectsMiddleSchool 冷启动 fallback 传初中年级必须被拒（建议与落库都不放行）。
func TestFrozenBypass_ColdStartRejectsMiddleSchool(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	for _, grade := range []string{"初一上", "初二下", "初三上"} {
		if _, err := d.InferProfile(ctx, "xiaoming", "小明", nil, grade, ""); err == nil {
			t.Fatalf("InferProfile fallback=%q 应被拒（冻结#2 绕过通道一）", grade)
		} else if !errors.Is(err, usecase.ErrInvalidInput) {
			t.Fatalf("应为 ErrInvalidInput，got %v", err)
		}
		if _, err := d.ColdStartProvision(ctx, "xiaoming", "小明", nil, grade, ""); err == nil {
			t.Fatalf("ColdStartProvision fallback=%q 应被拒", grade)
		}
	}
	// 小学档不受影响。
	if _, err := d.InferProfile(ctx, "xiaoming", "小明", nil, "五年级上", ""); err != nil {
		t.Fatalf("小学年级不应被拒: %v", err)
	}
}

// TestFrozenBypass_RestoreRejectsMiddleSchoolArchive 恢复含初中档案的归档必须整体拒绝。
func TestFrozenBypass_RestoreRejectsMiddleSchoolArchive(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()

	// 用合法导出构造归档，再注入初中档案重新签名——模拟「隐蔽通道」：归档结构完全合法、仅年级越界。
	bak, err := d.Backup(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	bak.Profile = &k12.ChildProfile{ChildName: "小明", GradeTerm: "初二上", TextbookEdition: "人教版"}
	// 注入后重签名：断言「即使校验和完全合法也要拦年级」——年级门独立于校验和门。
	bak.Checksum = usecase.HexbakChecksumForTest(bak)
	if _, err := d.Restore(ctx, bak); err == nil {
		t.Fatal("恢复含初中档案的归档应被拒（冻结#2 绕过通道二）")
	} else if !strings.Contains(err.Error(), "当前开放学段") {
		t.Fatalf("拒绝原因应指向学段，got %v", err)
	}
}
