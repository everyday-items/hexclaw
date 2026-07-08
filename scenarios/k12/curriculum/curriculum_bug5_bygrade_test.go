package curriculum_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
)

// BUG-5：删除死代码 byGrade（写后无读者 + 覆盖分支不清旧年级条目的双写脏数据）后，
// byKP 成为唯一真相。本测试钉死跨年级同名知识点的去重不变量——不管词表顺序如何
// （即便先出现较晚年级、再出现较早年级），FirstGrade 恒取**最早**首学年级，Size 去重。
func TestBUG5_CrossGradeEarliestFirstGradeAfterByGradeRemoval(t *testing.T) {
	ctx := context.Background()
	// 故意乱序：同一知识点先给较晚年级（五年级下），再给较早年级（四年级下）。
	c := curriculum.NewFromEntries("测试版", []curriculum.Entry{
		{KnowledgePoint: "观察物体", GradeTerm: "五年级下"},
		{KnowledgePoint: "观察物体", GradeTerm: "四年级下"},
		{KnowledgePoint: "小数乘法", GradeTerm: "五年级上"},
	})

	g, ok := c.FirstGrade(ctx, "观察物体")
	if !ok || g != "四年级下" {
		t.Errorf("跨年级同名知识点 FirstGrade 应取最早（四年级下），got %q ok=%v", g, ok)
	}
	// 去重：观察物体 + 小数乘法 = 2 条唯一。
	if c.Size() != 2 {
		t.Errorf("同名知识点应去重, Size=%d 期望 2", c.Size())
	}
	// 正查一致：五年级下应含已学的观察物体（首学四下 ≤ 五下）。
	allowed, err := c.Allowed(ctx, "五年级下")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, kp := range allowed {
		if kp == "观察物体" {
			found = true
		}
	}
	if !found {
		t.Errorf("五年级下 Allowed 应含观察物体（首学四下），got %v", allowed)
	}
}
