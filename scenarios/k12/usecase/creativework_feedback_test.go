package usecase_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/toolkit/util/logger"
)

func assertApprovedStructuredFeedbackProjection(
	t *testing.T,
	markdown, limitation string,
) {
	t.Helper()
	wantHeadings := []string{
		"## 可见证据",
		"## 先这样肯定",
		"## 家长可以这样问或讲",
		"## 下一次只试一个点",
	}
	gotHeadings := make([]string, 0, len(wantHeadings))
	for _, line := range strings.Split(markdown, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			gotHeadings = append(gotHeadings, line)
		}
	}
	if strings.Join(gotHeadings, "\x00") != strings.Join(wantHeadings, "\x00") {
		t.Fatalf("作品点评 H2 顺序 = %#v，期望 %#v；markdown=%q", gotHeadings, wantHeadings, markdown)
	}
	if strings.Contains(markdown, "## 说明") {
		t.Fatalf("能力限制只能作为四段后的轻量正文，不能形成第五个 H2：%q", markdown)
	}
	if strings.Count(markdown, limitation) != 1 {
		t.Fatalf("能力限制出现次数 = %d，期望 1；markdown=%q", strings.Count(markdown, limitation), markdown)
	}
	lastSection := strings.Index(markdown, "## 下一次只试一个点")
	limitationAt := strings.LastIndex(markdown, limitation)
	if lastSection < 0 || limitationAt <= lastSection {
		t.Fatalf("能力限制必须位于第四段之后：section=%d limitation=%d markdown=%q", lastSection, limitationAt, markdown)
	}
	if !strings.HasSuffix(strings.TrimSpace(markdown), limitation) {
		t.Fatalf("能力限制必须是投影最后一个非空内容块：%q", markdown)
	}
}

// fakeWorkFeedbackSolver 同时实现 Solver（占位）与 WorkFeedbackGenerator（fake executor），
// 契约测试用：可控输出/可控失败，并记录用例传给 Skill 层的生成请求。
type fakeWorkFeedbackSolver struct {
	feedback   string
	skillStamp string // 方法论基座来源戳（如 "writing-feedback@1.0.0/disk"），可为空
	err        error
	calls      int
	lastReq    usecase.WorkFeedbackRequest
	lastCtx    context.Context
}

func (f *fakeWorkFeedbackSolver) Solve(context.Context, string, string, string) (usecase.SolveResult, error) {
	return usecase.SolveResult{}, nil
}

func (f *fakeWorkFeedbackSolver) GenerateWorkFeedback(ctx context.Context, req usecase.WorkFeedbackRequest) (usecase.WorkFeedbackOutput, error) {
	f.calls++
	f.lastReq = req
	f.lastCtx = ctx
	return usecase.WorkFeedbackOutput{Feedback: f.feedback, SkillStamp: f.skillStamp}, f.err
}

func newWritingWork(t *testing.T, d usecase.Deps, agent string) string {
	t.Helper()
	id, _, err := d.CreateCreativeWork(context.Background(), agent, "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "《春天的校园》", Task: "观察春景写一段",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "柳枝像绿色的丝带，随风轻轻摆动。"}},
	})
	if err != nil {
		t.Fatalf("创建写作作品: %v", err)
	}
	return id
}

// TestGenerateWorkFeedback_Writing_AI 写作点评生成：draft → feedback_ready，
// 点评落最新版本且来源标记 ai（与家长手写区分）。
const writingFeedbackSixSectionResponse = "## 1. 一句话总评\n\n这是一篇切题准确、感情真实的习作，“熟悉又陌生”的感受很打动人，下一稿最值得加强的是让“爸爸的工作”和“爸爸对我的爱”衔接得更紧一些。\n\n## 2. 亮点\n\n### 亮点一：写出了对爸爸真实而复杂的感受\n\n原句：\n\n> “我们明明住在同一个家里，可我有时候却觉得爸爸既熟悉又陌生。”\n\n“熟悉”和“陌生”放在一起，很有新鲜感，也准确写出了爸爸常常加班给“我”带来的感受。后面又具体解释了：\n\n> “熟悉的是他的脚步声、说话的声音，还有那件常穿的外套；陌生的是，我不知道他每天在忙些什么。”\n\n这里没有只说“爸爸很忙”，而是写了“脚步声”“说话的声音”“常穿的外套”，有生活中的细节。  \n**建议收藏到积累**，以后写人物时可以继续使用“先写一种感受，再用几个生活细节解释”的方法。\n\n### 亮点二：把智能助手怎样帮助学习写清楚了\n\n原句：\n\n> “它没有马上告诉我答案，而是先帮我看懂题目，再按我学过的方法提醒我。”\n\n后面的两个问题——\n\n> “题目最后要求什么？”  \n> “第一步应该先算什么？”\n\n让读者看见了它是怎样一步一步提示的，不是空泛地说“它很有用”。这也为后文“不是替我做作业，而是在教我怎么思考”做好了铺垫。\n\n### 亮点三：结尾的愿望很真实\n\n原句：\n\n> “不过，我还是希望爸爸以后能少加一点班，多陪我打球、散步、聊天。”\n\n“打球、散步、聊天”都是朴素的小事，却比只写“希望爸爸多陪我”更具体，也让读者感受到孩子对爸爸的爱和想念。\n\n---\n\n## 3. 最值得改的三处\n\n### 第一处：从“不知道爸爸忙什么”过渡到“知道爸爸的工作”，可以再自然一些\n\n**原句：**\n\n> “后来我才知道，爸爸一直在开发一个叫‘河蟹AI’的智能助手。爸爸说，里面有很多不同的本领，叫作Skill。有的负责看题，有的负责讲题，还有的负责检查作业。”\n\n**问题与修改理由：**\n\n上一段刚写“我不知道他每天在忙些什么”，下一段直接进入“后来我才知道”，意思能接上，但“我是怎么知道的”没有点明。原文已经写了“爸爸说”，可以把这个信息提前，让前后衔接更顺畅，同时让“爸爸”继续站在文章中心。\n\n**参考改句：**\n\n> 后来，爸爸跟我说起他的工作，我才知道，他一直在开发一个叫“河蟹AI”的智能助手。这个助手有许多不同的本领，爸爸把它们叫作Skill：有的负责看题，有的负责讲题，还有的负责检查作业。\n\n**家长讲法：**\n\n先请孩子比较：“后来我才知道”和“后来，爸爸跟我说起他的工作，我才知道”，哪一句更能回答“你是怎么知道的”？\n\n可以追问：\n\n- 前一段的“陌生”，到这里发生了什么变化？\n- 是爸爸主动说起的，还是你问了以后他才说的？\n- 如果记不清具体情形，就不要添加，只保留原稿中已经确定的“爸爸说”。\n\n孩子卡住时，可以只让他补一个很小的信息：“谁告诉你的？”不必要求补出时间、地点或完整对话。\n\n检查理解时，让孩子用自己的话说一说：“为什么补上‘爸爸跟我说起他的工作’后，前后更连贯？”再请他找出文中另一处表示前后变化的词，如“这时候我才明白”。\n\n---\n\n### 第二处：解题过程已经清楚，可以把句子之间的先后关系写得更紧凑\n\n**原句：**\n\n> “我想不出来的时候，它就把难题拆成几个小问题，一步一步提示我。做完以后，它还帮我找到第一处错误，又给我出了一道相似的题，让我再练一次。”\n\n**问题与修改理由：**\n\n这里有“想不出来—得到提示—完成题目—发现错误—再次练习”几个步骤，内容很好，但“做完以后”前面没有明确说“我按照提示做”，读者需要自己补想。稍微补出这个连接，过程会更清楚。\n\n**参考改句：**\n\n> 我一时答不上来，它就把难题拆成几个小问题，一步一步提示我。我按照提示做完后，它又帮我找出第一处错误，并给我出了一道相似的题，让我再练一次。\n\n**家长讲法：**\n\n请孩子先圈出这一段中的几个动作：\n\n- 想不出来\n- 拆成小问题\n- 提示\n- 做完\n- 找出错误\n- 再练一次\n\n再问：“从‘提示我’到‘做完以后’，中间是谁按照提示做了题？”孩子一般能发现，需要把“我按照提示做”补出来。\n\n如果孩子想把这里写得更具体，可以继续追问：\n\n- 听到“题目最后要求什么”时，你有没有重新读题？\n- 听到“第一步应该先算什么”时，你是马上想出来了，还是又想了一会儿？\n- 当时真实发生了什么，就补什么；没有发生过的动作不要添加。\n\n检查理解时，请孩子解释：“修改后，读者为什么更容易看懂解题顺序？”然后让他在别的段落中找一组有先后顺序的动作，尝试用“先……再……最后……”口头复述。\n\n---\n\n### 第三处：“爸爸辛苦”和“爸爸爱我”之间，可以用前文的事实搭一座桥\n\n**原句：**\n\n> “我知道爸爸工作很辛苦，也是在用另一种方式爱我。不过，我还是希望爸爸以后能少加一点班，多陪我打球、散步、聊天。”\n\n**问题与修改理由：**\n\n“爸爸很辛苦”和“爸爸爱我”都是真实感受，但“另一种方式”稍显概括。可以把前文的两个事实——“加班到很晚”和“智能助手一步一步教我思考”——带进来，让这个感悟更有依据，也更能突出题目中的“好爸爸”。\n\n**参考改句：**\n\n> 想到爸爸常常加班到很晚，又想到河蟹AI一步一步教我解题，我明白了：爸爸的辛苦里，也藏着他对我的爱。不过，我还是希望爸爸以后能少加一点班，多陪我打球、散步、聊天。\n\n**家长讲法：**\n\n先问孩子：“你为什么会觉得爸爸是在用另一种方式爱你？”不要急着给答案，让孩子回到前文找证据。\n\n可以继续追问：\n\n- 哪一句写了爸爸工作辛苦？\n- 哪一件事让你感受到爸爸的工作也帮助了你？\n- “爱我”前面至少需要哪两个事实来支撑？\n\n如果孩子卡住，就把范围缩小到第一段和解题部分，让他分别找一句。找到后，再用“想到……又想到……我明白了……”把两处内容连起来。\n\n检查理解时，不只问“懂了吗”，而要请孩子说出：“感受前面为什么要有具体事情？”然后让他试着把“我很开心”口头改成“因为发生了什么，所以我很开心”。\n\n---\n\n### 家长参考稿\n\n> 以下参考稿只整理和连接原稿中已经出现的内容，没有补写新的经历。建议家长先让孩子自己修改，再用来比较，不要求孩子照抄。\n\n# 我的好爸爸\n\n我的爸爸是一名程序员。他每天工作都很忙，经常加班到很晚。有时候我已经睡着了，他才轻轻地回到家。第二天早上，我背着书包去上学的时候，爸爸可能还在睡觉，因为他前一天工作得太晚了。\n\n我们明明住在同一个家里，可我有时候却觉得爸爸既熟悉又陌生。熟悉的是他的脚步声、说话的声音，还有那件常穿的外套；陌生的是，我不知道他每天在忙些什么。\n\n后来，爸爸跟我说起他的工作，我才知道，他一直在开发一个叫“河蟹AI”的智能助手。这个助手有许多不同的本领，爸爸把它们叫作Skill：有的负责看题，有的负责讲题，还有的负责检查作业。\n\n有一次，我遇到一道不会做的数学题，便请河蟹AI帮我看看。它没有马上告诉我答案，而是先帮我看懂题目，再按我学过的方法提醒我。它先问我：“题目最后要求什么？”又问我：“第一步应该先算什么？”\n\n我一时答不上来，它就把难题拆成几个小问题，一步一步提示我。我按照提示做完后，它又帮我找出第一处错误，并给我出了一道相似的题，让我再练一次。\n\n这时候我才明白，河蟹AI不是替我做作业，而是在教我怎么思考。爸爸把许多老师的好方法放进这些Skill里，让我学会先读题、再动脑、最后检查。\n\n想到爸爸常常加班到很晚，又想到河蟹AI一步一步教我解题，我明白了：爸爸的辛苦里，也藏着他对我的爱。不过，我还是希望爸爸以后能少加一点班，多陪我打球、散步、聊天。\n\n在我的心里，爸爸虽然只是一名普通的程序员，却是我最爱的好爸爸。\n\n这份参考稿主要整合了三个方法：**补清过渡、写顺事情的步骤、用前文事实支撑结尾的感受**。\n\n---\n\n## 4. 基础规范清单\n\n### 标点候选\n\n- **位置：第三段**\n- **原样引用：**“一个叫‘河蟹AI’的智能助手”\n- 中文文章中，名称或需要特别指出的词语通常使用中文双引号，可改为：  \n  “一个叫‘河蟹AI’的智能助手”\n- **错题候选·待家长确认**\n\n除这一处引号形式外，本稿未发现明显的错别字或标点问题。“Skill”如果是产品中的固定名称，可以保留原写法。\n\n---\n\n## 5. 下一步小任务\n\n**只重写倒数第二段，用前文的两个具体事实说明“爸爸是怎样用另一种方式爱我”的。**\n\n可以沿用这个句式，但不要机械照抄：\n\n> 想到________，又想到________，我明白了：________。\n\n**家长检查标准：**\n\n1. 两个空格中填写的都是原稿里真实写过的事情；\n2. 结尾的感受能从这两件事中自然得出；\n3. 修改后的段落与前后的内容连得上；\n4. 孩子能指出自己新增或补清了哪两个依据。\n\n---\n\n## 6. 家长怎么带着改\n\n1. **先讲亮点。**  \n   先读“既熟悉又陌生”这一段，告诉孩子：“这里的感受很特别，而且你用脚步声、说话声和外套解释清楚了，值得保留。”\n\n2. **再只看倒数第二段。**  \n   问：“你说爸爸在用另一种方式爱你，前文哪两件事能证明？”让孩子自己回原文圈句子。\n\n3. **孩子卡住时缩小范围。**  \n   提示他只看第一段和第四至第六段，分别找“爸爸很辛苦”和“爸爸的工作怎样帮助我”的内容。\n\n4. **先口头说，再动笔。**  \n   让孩子先完整说一遍：“想到……又想到……我明白了……”说顺以后，再写进作文。\n\n5. **读前后文检查连接。**  \n   从“这时候我才明白”一直读到最后，听一听“明白”是否重复得太近。若孩子觉得重复，可以把后一处换成“我懂得了”或调整句子。\n\n6. **检查是否真正理解。**  \n   请孩子不用参考稿解释：“为什么不能只写‘爸爸很爱我’，而要把前面的事情带进来？”最后让他独立修改第一处过渡句，看看能否把同样的“前后连接”方法用到另一处。"

func TestGenerateWorkFeedback_Writing_AI(t *testing.T) {
	d := newDataDeps(t)
	d.WorkFeedbackRoute = func(context.Context, string) (k12.ImageTaskRouteSnapshot, error) {
		route := currentFeedbackRoute()
		route.TimeoutMS = 0
		return route, nil
	}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	gen := &fakeWorkFeedbackSolver{feedback: "## 可见证据\n\n这是一篇切题准确、感情真实的习作，“熟悉又陌生”的感受很打动人，下一稿最值得加强的是让“爸爸的工作”和“爸爸对我的爱”衔接得更紧一些。\n\n## 先这样肯定\n\n原句：\n\n> “我们明明住在同一个家里，可我有时候却觉得爸爸既熟悉又陌生。”\n\n“熟悉”和“陌生”放在一起，很有新鲜感，也准确写出了爸爸常常加班给“我”带来的感受。后面又具体解释了：\n\n> “熟悉的是他的脚步声、说话的声音，还有那件常穿的外套；陌生的是，我不知道他每天在忙些什么。”\n\n这里没有只说“爸爸很忙”，而是写了“脚步声”“说话的声音”“常穿的外套”，有生活中的细节。\n\n## 家长可以这样问或讲\n\n**原句：**\n\n> “我知道爸爸工作很辛苦，也是在用另一种方式爱我。不过，我还是希望爸爸以后能少加一点班，多陪我打球、散步、聊天。”\n\n**问题与修改理由：**\n\n“爸爸很辛苦”和“爸爸爱我”都是真实感受，但“另一种方式”稍显概括。可以把前文的两个事实——“加班到很晚”和“智能助手一步一步教我思考”——带进来，让这个感悟更有依据，也更能突出题目中的“好爸爸”。\n\n**参考改句：**\n\n> 想到爸爸常常加班到很晚，又想到河蟹AI一步一步教我解题，我明白了：爸爸的辛苦里，也藏着他对我的爱。不过，我还是希望爸爸以后能少加一点班，多陪我打球、散步、聊天。\n\n**家长讲法：**\n\n先问孩子：“你为什么会觉得爸爸是在用另一种方式爱你？”不要急着给答案，让孩子回到前文找证据。\n\n可以继续追问：\n\n- 哪一句写了爸爸工作辛苦？\n- 哪一件事让你感受到爸爸的工作也帮助了你？\n- “爱我”前面至少需要哪两个事实来支撑？\n\n如果孩子卡住，就把范围缩小到第一段和解题部分，让他分别找一句。找到后，再用“想到……又想到……我明白了……”把两处内容连起来。\n\n检查理解时，不只问“懂了吗”，而要请孩子说出：“感受前面为什么要有具体事情？”然后让他试着把“我很开心”口头改成“因为发生了什么，所以我很开心”。\n\n---\n\n### 家长参考稿\n\n> 以下参考稿只整理和连接原稿中已经出现的内容，没有补写新的经历。建议家长先让孩子自己修改，再用来比较，不要求孩子照抄。\n\n### 我的好爸爸\n\n我的爸爸是一名程序员。他每天工作都很忙，经常加班到很晚。有时候我已经睡着了，他才轻轻地回到家。第二天早上，我背着书包去上学的时候，爸爸可能还在睡觉，因为他前一天工作得太晚了。\n\n我们明明住在同一个家里，可我有时候却觉得爸爸既熟悉又陌生。熟悉的是他的脚步声、说话的声音，还有那件常穿的外套；陌生的是，我不知道他每天在忙些什么。\n\n后来，爸爸跟我说起他的工作，我才知道，他一直在开发一个叫“河蟹AI”的智能助手。这个助手有许多不同的本领，爸爸把它们叫作Skill：有的负责看题，有的负责讲题，还有的负责检查作业。\n\n有一次，我遇到一道不会做的数学题，便请河蟹AI帮我看看。它没有马上告诉我答案，而是先帮我看懂题目，再按我学过的方法提醒我。它先问我：“题目最后要求什么？”又问我：“第一步应该先算什么？”\n\n我一时答不上来，它就把难题拆成几个小问题，一步一步提示我。我按照提示做完后，它又帮我找出第一处错误，并给我出了一道相似的题，让我再练一次。\n\n这时候我才明白，河蟹AI不是替我做作业，而是在教我怎么思考。爸爸把许多老师的好方法放进这些Skill里，让我学会先读题、再动脑、最后检查。\n\n想到爸爸常常加班到很晚，又想到河蟹AI一步一步教我解题，我明白了：爸爸的辛苦里，也藏着他对我的爱。不过，我还是希望爸爸以后能少加一点班，多陪我打球、散步、聊天。\n\n在我的心里，爸爸虽然只是一名普通的程序员，却是我最爱的好爸爸。\n\n这份参考稿主要整合了三个方法：**补清过渡、写顺事情的步骤、用前文事实支撑结尾的感受**。\n\n\n### 家长讲法\n\n1. **先讲亮点。**  \n   先读“既熟悉又陌生”这一段，告诉孩子：“这里的感受很特别，而且你用脚步声、说话声和外套解释清楚了，值得保留。”\n\n2. **再只看倒数第二段。**  \n   问：“你说爸爸在用另一种方式爱你，前文哪两件事能证明？”让孩子自己回原文圈句子。\n\n3. **孩子卡住时缩小范围。**  \n   提示他只看第一段和第四至第六段，分别找“爸爸很辛苦”和“爸爸的工作怎样帮助我”的内容。\n\n4. **先口头说，再动笔。**  \n   让孩子先完整说一遍：“想到……又想到……我明白了……”说顺以后，再写进作文。\n\n5. **读前后文检查连接。**  \n   从“这时候我才明白”一直读到最后，听一听“明白”是否重复得太近。若孩子觉得重复，可以把后一处换成“我懂得了”或调整句子。\n\n6. **检查是否真正理解。**  \n   请孩子不用参考稿解释：“为什么不能只写‘爸爸很爱我’，而要把前面的事情带进来？”最后让他独立修改第一处过渡句，看看能否把同样的“前后连接”方法用到另一处。\n\n## 下一次只试一个点\n\n**只重写倒数第二段，用前文的两个具体事实说明“爸爸是怎样用另一种方式爱我”的。**\n\n可以沿用这个句式，但不要机械照抄：\n\n> 想到________，又想到________，我明白了：________。\n\n**家长检查标准：**\n\n1. 两个空格中填写的都是原稿里真实写过的事情；\n2. 结尾的感受能从这两件事中自然得出；\n3. 修改后的段落与前后的内容连得上；\n4. 孩子能指出自己新增或补清了哪两个依据。\n"}
	d.Solver = gen
	ctx := context.Background()
	sourceContent := "我的好爸爸\n\n我的爸爸是一名程序员。他每天工作都很忙，经常加班到很晚。有时候我已经睡着了，他才轻轻地回到家。第二天早上，我背着书包去上学的时候，爸爸可能还在睡觉，因为他前一天工作得太晚了。\n\n我们明明住在同一个家里，可我有时候却觉得爸爸既熟悉又陌生。熟悉的是他的脚步声、说话的声音，还有那件常穿的外套；陌生的是，我不知道他每天在忙些什么。\n\n后来我才知道，爸爸一直在开发一个叫‘河蟹AI’的智能助手。爸爸说，里面有很多不同的本领，叫作Skill。有的负责看题，有的负责讲题，还有的负责检查作业。\n\n有一次，我遇到一道不会做的数学题。我把题目交给河蟹AI，它没有马上告诉我答案，而是先帮我看懂题目，再按我学过的方法提醒我。它先问我：“题目最后要求什么？”又问我：“第一步应该先算什么？”\n\n我想不出来的时候，它就把难题拆成几个小问题，一步一步提示我。做完以后，它还帮我找到第一处错误，又给我出了一道相似的题，让我再练一次。\n\n这时候我才明白，河蟹AI不是替我做作业，而是在教我怎么思考。爸爸把很多老师的好方法放进了这些Skill里，让我学会先读题、再动脑、最后检查。\n\n我知道爸爸工作很辛苦，也是在用另一种方式爱我。不过，我还是希望爸爸以后能少加一点班，多陪我打球、散步、聊天。\n\n在我的心里，爸爸虽然只是一个普通的程序员，却是我最爱的好爸爸。"
	id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "我的好爸爸",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: sourceContent}},
	})
	if err != nil {
		t.Fatal(err)
	}

	feedbackStartedAt := time.Now()
	v, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
	if err != nil {
		t.Fatalf("生成点评: %v", err)
	}
	logs.Reset()
	logger.FromContext(gen.lastCtx).Info("writing provider context log")
	var logEntry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &logEntry); err != nil {
		t.Fatalf("provider context log did not reach the current collector: %v", err)
	}
	if logEntry["agent_id"] != "xiaoming" || logEntry["work_id"] != id ||
		logEntry["generation_id"] == nil || logEntry["generation_id"] == "" ||
		logEntry["invocation_id"] == nil || logEntry["invocation_id"] == "" ||
		logEntry["stage"] != "work_feedback" {
		t.Fatalf("provider context log missing feedback correlation: %+v", logEntry)
	}
	invocation, err := d.Records.GetLatestWorkFeedbackInvocation(
		ctx, "xiaoming", id, "work:"+id+":version:"+v.Fields.Versions[0].VersionID+":feedback",
	)
	if err != nil {
		t.Fatal(err)
	}
	providerDeadline, hasDeadline := gen.lastCtx.Deadline()
	if !hasDeadline || providerDeadline.Unix() != invocation.DeadlineAt ||
		invocation.DeadlineAt < feedbackStartedAt.Add(299*time.Second).Unix() ||
		invocation.DeadlineAt > feedbackStartedAt.Add(300*time.Second).Unix() {
		t.Fatalf("independent feedback budget is not frozen in its invocation: deadline=%v invocation=%+v", providerDeadline, invocation)
	}
	if v.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("生成点评后应为 feedback_ready，got %s", v.Record.Status)
	}
	last := v.Fields.Versions[len(v.Fields.Versions)-1]
	if last.Feedback == gen.feedback || !strings.Contains(last.Feedback, "## 可见证据") {
		t.Fatalf("点评应写入由 canonical facts 生成的确定性投影，got %q", last.Feedback)
	}
	if last.FeedbackSource != k12.FeedbackSourceAI {
		t.Fatalf("AI 点评来源应为 %q，got %q", k12.FeedbackSourceAI, last.FeedbackSource)
	}
	if last.StructuredFeedback == nil {
		t.Fatal("AI 点评必须持久化 canonical structured feedback，不能只存自由 Markdown")
	}
	if err := last.StructuredFeedback.Validate(); err != nil {
		t.Fatalf("structured feedback invalid: %v (%#v)", err, last.StructuredFeedback)
	}
	if len(last.StructuredFeedback.EvidenceRefs) == 0 || len(last.StructuredFeedback.Suggestions) < 1 || len(last.StructuredFeedback.Suggestions) > 3 {
		t.Fatalf("structured feedback evidence/suggestions incomplete: %#v", last.StructuredFeedback)
	}
	if last.StructuredFeedback.SourceSnapshot.Source != k12.FeedbackSourceAI ||
		last.StructuredFeedback.Limitations != "观察依据为孩子原稿；修改示范与参考稿供家长辅导使用。" {
		t.Fatalf("structured feedback source/limitations incomplete: %#v", last.StructuredFeedback)
	}
	if last.StructuredFeedback.FeedbackID == "" || last.StructuredFeedback.VersionID != "v1" ||
		last.StructuredFeedback.FeedbackType != k12.WorkTypeWriting ||
		last.StructuredFeedback.SourceSnapshot.Source != k12.FeedbackSourceAI ||
		last.StructuredFeedback.ProjectionMarkdown == "" {
		t.Fatalf("canonical feedback identity/type/source/projection incomplete: %#v", last.StructuredFeedback)
	}
	// 生成请求必须携带作品可见证据（题目要求 + 原文），不发明输入。
	if gen.lastReq.WorkType != k12.WorkTypeWriting ||
		gen.lastReq.ContentMarkdown != sourceContent {
		t.Fatalf("生成请求缺少作品证据: %+v", gen.lastReq)
	}
	if last.ContentMarkdown != sourceContent ||
		!strings.Contains(last.StructuredFeedback.Affirmation, "熟悉又陌生") ||
		!strings.Contains(last.StructuredFeedback.ParentGuidance, "### 我的好爸爸\n\n") ||
		!strings.Contains(last.StructuredFeedback.ParentGuidance, "却是我最爱的好爸爸。") ||
		!strings.Contains(last.StructuredFeedback.ParentGuidance, "先讲亮点") ||
		!strings.Contains(last.StructuredFeedback.NextStep, "只重写倒数第二段") ||
		!strings.Contains(last.Feedback, last.StructuredFeedback.ParentGuidance) {
		t.Fatalf("writing feedback lost its original source, parent method or complete reference: %+v", last.StructuredFeedback)
	}
	assertApprovedStructuredFeedbackProjection(t, last.Feedback, last.StructuredFeedback.Limitations)
}

// TestGenerateWorkFeedback_SkillStampPersisted 追溯契约：生成器申报的方法论基座来源戳
// 随点评落库到版本记录 feedback_skill 字段（能查到每条点评用的哪版方法论）。
func TestGenerateWorkFeedback_SkillStampPersisted(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"盘上版本", "writing-feedback@1.2.0/disk", "writing-feedback@1.2.0/disk"},
		{"内嵌快照", "writing-feedback@1.0.0/embedded", "writing-feedback@1.0.0/embedded"},
		{"硬编码兜底", "builtin", "builtin"},
		{"未申报时诚实标记", "", "unreported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDataDeps(t)
			d.Solver = &fakeWorkFeedbackSolver{
				feedback:   "「柳枝像绿色的丝带」比喻好；建议结尾补一个听觉细节。",
				skillStamp: tc.input,
			}
			ctx := context.Background()
			id := newWritingWork(t, d, "xiaoming")
			v, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
			if err != nil {
				t.Fatalf("生成点评: %v", err)
			}
			last := v.Fields.Versions[len(v.Fields.Versions)-1]
			if last.FeedbackSkill != tc.want {
				t.Fatalf("feedback_skill 应落库 %q，got %q", tc.want, last.FeedbackSkill)
			}
		})
	}
}

// TestGenerateWorkFeedback_INV011_Rejected 保留评分边界，允许家长参考稿且不覆盖原稿。
func TestGenerateWorkFeedback_INV011_Rejected(t *testing.T) {
	cases := []struct {
		name, out string
		allowed   bool
	}{
		{"打分", "整体不错，可以打 90 分。", false},
		{"满分口径", "距离满分只差一点点。", false},
		{"等第", "这篇作文可评为优等。", false},
		{"家长参考范文", "## 可见证据\n原稿写了柳枝像绿色的丝带。\n## 先这样肯定\n柳枝像绿色的丝带，比喻贴切。\n## 家长可以这样问或讲\n先比较柳枝和丝带的形状，再请孩子解释相似处。\n\n### 家长参考范文如下\n春天来了，校园里的柳枝像绿色的丝带。\n## 下一次只试一个点\n请孩子解释相似处，再写另一种植物。", true},
		{"旧六段参考稿缺少四段合同", writingFeedbackSixSectionResponse, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDataDeps(t)
			d.Solver = &fakeWorkFeedbackSolver{feedback: tc.out}
			ctx := context.Background()
			id := newWritingWork(t, d, "xiaoming")
			before, err := d.GetCreativeWork(ctx, "xiaoming", id)
			if err != nil {
				t.Fatal(err)
			}

			generated, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
			if tc.allowed {
				if err != nil {
					t.Fatalf("家长参考稿不应被拒: %v", err)
				}
				last := generated.Fields.Versions[0]
				if generated.Record.Status != k12.WorkStatusFeedbackReady ||
					last.ContentMarkdown != before.Fields.Versions[0].ContentMarkdown ||
					!strings.Contains(last.Feedback, "家长参考范文如下") {
					t.Fatalf("参考稿应保留在点评且不得覆盖原稿: %+v", generated)
				}
				return
			}
			if err == nil {
				t.Fatalf("输出 %q 违反 INV-011，应拒绝", tc.out)
			}
			v, _ := d.GetCreativeWork(ctx, "xiaoming", id)
			if v.Record.Status != k12.WorkStatusDraft {
				t.Fatalf("拒绝后作品应保持 draft，got %s", v.Record.Status)
			}
			if v.Fields.Versions[0].Feedback != "" || v.Fields.Versions[0].FeedbackSource != "" {
				t.Fatalf("拒绝入库不得残留点评: %+v", v.Fields.Versions[0])
			}
		})
	}
}

// TestGenerateWorkFeedback_FeedbackNotFalselyRejected 正常点评不应被 INV-011 误杀
// （如“10分钟”“百分数”这类含“分”字但非打分的表述）。
func TestGenerateWorkFeedback_FeedbackNotFalselyRejected(t *testing.T) {
	d := newDataDeps(t)
	d.Solver = &fakeWorkFeedbackSolver{feedback: "「柳枝像绿色的丝带」比喻好；建议每天花 10 分钟朗读，结尾再补一个细节。"}
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err != nil {
		t.Fatalf("“10 分钟”不是打分，不应被拒: %v", err)
	}
}

// BUG-20260726-003: real vision feedback can contain many short, valid observations.
// Packing every observation after the second into one row used to exceed the canonical
// 500-rune atom limit and discard the entire first feedback generation.
func TestBUG20260726003_GenerateWorkFeedback_PacksVerboseEvidenceIntoValidAtoms(t *testing.T) {
	d := newDataDeps(t)
	var raw strings.Builder
	labels := []rune("甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未申酉戌亥天地玄黄宇宙洪荒日月盈昃辰宿列张")
	for i := 0; i < 36; i++ {
		raw.WriteRune(labels[i])
		raw.WriteString("原文写出了爸爸下班后仍陪孩子分析题目的具体动作，人物关系清楚。")
	}
	raw.WriteString("建议下一次只补充一处能听见的生活细节。")
	d.Solver = &fakeWorkFeedbackSolver{feedback: raw.String()}
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")

	view, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
	if err != nil {
		t.Fatalf("有效但详细的真实模型点评不得因内部装箱超过 500 字而整单失败: %v", err)
	}
	feedback := view.Fields.Versions[0].StructuredFeedback
	if feedback == nil {
		t.Fatal("详细点评必须生成 canonical structured feedback")
	}
	if len(feedback.Observations) < 1 || len(feedback.Observations) > 3 {
		t.Fatalf("观察仍须保持 1-3 条，got %d", len(feedback.Observations))
	}
	for _, observation := range feedback.Observations {
		if got := len([]rune(observation.Evidence)); got > 500 {
			t.Fatalf("观察原子仍超过 500 字: %d", got)
		}
	}
	var packed strings.Builder
	for _, observation := range feedback.Observations {
		packed.WriteString(observation.Evidence)
	}
	if !strings.Contains(packed.String(), "甲原文") || !strings.Contains(packed.String(), "宿原文") {
		t.Fatalf("装箱不得静默丢掉首尾有效证据: %q", packed.String())
	}
}

func TestBUG20260726003_GenerateWorkFeedback_SplitsOneOversizedEvidenceAtom(t *testing.T) {
	d := newDataDeps(t)
	raw := strings.Repeat("原文里能看到爸爸陪孩子分析题目的具体动作", 40) +
		"收尾证据。建议下一次只补充一处能听见的生活细节。"
	d.Solver = &fakeWorkFeedbackSolver{feedback: raw}
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")

	view, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
	if err != nil {
		t.Fatalf("单条有效观察较长时应拆成 canonical atoms，而不是整单失败: %v", err)
	}
	feedback := view.Fields.Versions[0].StructuredFeedback
	if feedback == nil {
		t.Fatal("长观察必须生成 canonical structured feedback")
	}
	var packed strings.Builder
	for _, observation := range feedback.Observations {
		if got := len([]rune(observation.Evidence)); got > 500 {
			t.Fatalf("拆分后观察原子仍超过 500 字: %d", got)
		}
		packed.WriteString(observation.Evidence)
	}
	if !strings.Contains(packed.String(), "收尾证据") {
		t.Fatalf("拆分不得丢失长观察末尾的有效证据: %q", packed.String())
	}
}

// 美术生成请求携带原图和意图，但未遵守四段合同的输出不能成为成功点评。
func TestGenerateWorkFeedback_Art_Observational(t *testing.T) {
	d := newDataDeps(t)
	gen := &fakeWorkFeedbackSolver{feedback: "### 我在画里看到的\n\n画面中央是一位正面站立、举起左手的女孩。她有棕色长发，头上戴着粉色蝴蝶结，穿紫色爱心上衣、蓝色百褶裙和粉色鞋子。眼睛里画出了黑色瞳孔、蓝色虹膜和多个白色高光，脸颊还有粉色短线。\n\n女孩右下方坐着一只橙色小猫，身上有棕色条纹，脖子上戴着粉色项圈和黄色圆牌。左上角是带两朵云的彩虹，周围分布着不同颜色的爱心和星星。女孩和小猫脚下都有浅绿色线条，像是把它们放在同一块地面上。整张画主要使用了彩色铅笔或蜡笔一类能留下笔触的材料，涂色纹理清楚可见。\n\n### 画面里很值得注意的地方\n\n- **中心人物很醒目。**女孩占据了画面中间最大的位置，举起的手臂又向左上方伸展，把视线带向彩虹；右下方的小猫则让另一侧不显得空。这说明你已经会用大小、位置和动作来安排画面的主次。\n- **细节是有选择地加进去的。**眼睛里的蓝色、黑色和白色高光分得很清楚；裙子的褶线、猫的条纹、项圈和圆牌也都能辨认出来。到了六年级，开始主动观察并增加这些局部细节，是写实观察正在发展的表现。\n- **颜色之间有呼应。**蝴蝶结、衣服上的爱心、鞋子和右上角爱心都用了粉色；紫色也重复出现在衣服、爱心、星星和鞋面蝴蝶结上。这样的重复让分散的小图形和中央人物产生了联系。\n- **线条保留了手绘的活力。**头发和衣服里的彩色笔触方向清楚，没有被完全磨平，因此画面既整齐，又还能看见你画画时手的运动。\n\n### 下次可以试试的小实验\n\n1. **试试让手部动作更有观察感。**  \n   先把自己的手摆成画中张开的姿势，用眼睛沿手掌外轮廓慢慢看一圈，再注意每根手指伸出的方向和长短。可以用五分钟画三只不同姿势的手，每只只画外轮廓，比一比哪一只最像当时看到的形状。\n\n2. **试试用遮挡增加前后距离。**  \n   这张画里的女孩和小猫大多完整地并排出现。你可以拿三个小纸片分别当作人物、动物和装饰，前后移动，让其中一个挡住另一个的一小部分，再选择一种排列快速画下来。五分钟换两三种位置，比一比哪一种最有前后空间。\n\n3. **试试用同一种颜色画出深浅变化。**  \n   可以选头发的一小块，用同一支棕色笔分别轻画一层、叠画两层、加重画三层，做成三条色带；再看看把哪一种深浅放在头发边缘、发束交叠处或蝴蝶结下面，会让形状更清楚。这只是材料实验，不需要把整张画重新涂一遍。\n\n这次没有提供具体的创作任务或你想讲的故事，所以我只根据画面中看得见的构图、颜色、线条和细节来点评，没有替你猜作品的情节，也没有评价任务完成度。"}
	d.Solver = gen
	ctx := context.Background()
	id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "《雨后的校园》", Task: "画一处雨后场景",
		Intent:   "想画出雨后安静的感觉",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: "asset-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("unstructured art output must fail the existing contract: %v", err)
	}
	v, err := d.GetCreativeWork(ctx, "xiaoming", id)
	if err != nil || v.GenerationState.Initial == nil || v.GenerationState.Initial.Status != "failed" || v.GenerationState.Latest != nil {
		t.Fatalf("invalid art output must not leave a successful projection: %+v err=%v", v.GenerationState, err)
	}
	if gen.lastReq.WorkType != k12.WorkTypeArt || gen.lastReq.Intent == "" || gen.lastReq.SourceAssetID != "asset-1" {
		t.Fatalf("美术生成请求缺少可见证据来源: %+v", gen.lastReq)
	}
}

// TestGenerateWorkFeedback_Art_StructuredProjectionPreservesAllVisualEvidence
// locks the structure-first projection boundary: Markdown scaffolding must not consume
// one of the three observation slots, and later real image evidence must not be dropped.
func TestGenerateWorkFeedback_Art_StructuredProjectionPreservesAllVisualEvidence(t *testing.T) {
	d := newDataDeps(t)
	gen := &fakeWorkFeedbackSolver{feedback: `## 可见证据
人物在画面中央，占据最大的面积。
右下方有一只橙色小猫，尾巴向上弯。
左上方还可以看到带白云的明亮彩虹。底部有绿色地面和小草。
## 先这样肯定
人物最大，小猫和彩虹围绕它，主次安排清楚。
## 家长可以这样问或讲
请孩子先指出人物看向哪里，再观察视线怎样和小猫发生联系。
## 下一次只试一个点
用五分钟画三种人物看向小猫的方向，比一比视线变化。`}
	d.Solver = gen
	ctx := context.Background()
	id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "《彩虹和小猫》", Task: "观察人物、猫、彩虹和地面的构图",
		Intent:   "想画快乐的户外场景",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: "asset-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
	if err != nil {
		t.Fatalf("美术点评生成: %v", err)
	}
	structured := v.Fields.Versions[0].StructuredFeedback
	if structured == nil {
		t.Fatal("结构化点评缺失")
	}
	if len(structured.Observations) > 3 {
		t.Fatalf("结构化观察仍应保持最多 3 条，got %d", len(structured.Observations))
	}
	var evidence strings.Builder
	for _, observation := range structured.Observations {
		evidence.WriteString(observation.Evidence)
		evidence.WriteByte('\n')
	}
	for _, want := range []string{"人物", "小猫", "彩虹", "地面"} {
		if !strings.Contains(evidence.String(), want) {
			t.Errorf("结构化观察丢失真实画面证据 %q: %q", want, evidence.String())
		}
	}
	if strings.Contains(evidence.String(), "我在画里看到") || strings.Contains(evidence.String(), "观察与依据") {
		t.Fatalf("Markdown 标题/占位语不应占 observation: %q", evidence.String())
	}
}

func TestGenerateWorkFeedback_Art_StructuredProjectionKeepsSuggestionsOutOfObservations(t *testing.T) {
	d := newDataDeps(t)
	d.Solver = &fakeWorkFeedbackSolver{feedback: "## 可见证据\n\n画面中央是一位正面站立、举起左手的女孩。她有棕色长发，头上戴着粉色蝴蝶结，穿紫色爱心上衣、蓝色百褶裙和粉色鞋子。眼睛里画出了黑色瞳孔、蓝色虹膜和多个白色高光，脸颊还有粉色短线。\n\n女孩右下方坐着一只橙色小猫，身上有棕色条纹，脖子上戴着粉色项圈和黄色圆牌。左上角是带两朵云的彩虹，周围分布着不同颜色的爱心和星星。女孩和小猫脚下都有浅绿色线条，像是把它们放在同一块地面上。整张画主要使用了彩色铅笔或蜡笔一类能留下笔触的材料，涂色纹理清楚可见。\n\n## 先这样肯定\n\n**中心人物很醒目。**女孩占据了画面中间最大的位置，举起的手臂又向左上方伸展，把视线带向彩虹；右下方的小猫则让另一侧不显得空。这说明你已经会用大小、位置和动作来安排画面的主次。\n\n## 家长可以这样问或讲\n\n先把自己的手摆成画中张开的姿势，用眼睛沿手掌外轮廓慢慢看一圈，再注意每根手指伸出的方向和长短。\n\n## 下一次只试一个点\n\n可以用五分钟画三只不同姿势的手，每只只画外轮廓，比一比哪一只最像当时看到的形状。"}
	ctx := context.Background()
	id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "《彩虹和小猫》", Task: "观察人物、猫、彩虹和地面的构图",
		Versions: []k12.CreativeWorkVersion{{SourceAssetID: "asset-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.GenerateWorkFeedback(ctx, "xiaoming", id)
	if err != nil {
		t.Fatalf("美术点评生成: %v", err)
	}
	structured := v.Fields.Versions[0].StructuredFeedback
	if structured == nil {
		t.Fatal("结构化点评缺失")
	}
	joinedSuggestions := strings.Join(structured.Suggestions, "\n")
	for _, want := range []string{"用眼睛沿手掌外轮廓慢慢看一圈", "用五分钟画三只不同姿势的手"} {
		if !strings.Contains(joinedSuggestions, want) {
			t.Errorf("真实建议被误归为观察，missing %q in suggestions=%q observations=%#v", want, joinedSuggestions, structured.Observations)
		}
	}
	joinedObservations := ""
	for _, observation := range structured.Observations {
		joinedObservations += observation.Evidence + "\n"
	}
	if !strings.Contains(joinedObservations, "画面中央") || strings.Contains(joinedObservations, "分钟练习") {
		t.Fatalf("可见事实与练习必须分开: %q", joinedObservations)
	}
	raw, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	var fact map[string]any
	if err := json.Unmarshal(raw, &fact); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"affirmation":     "中心人物很醒目",
		"parent_guidance": "用眼睛沿手掌外轮廓慢慢看一圈",
		"next_step":       "用五分钟画三只不同姿势的手",
	} {
		value, _ := fact[field].(string)
		if !strings.Contains(value, want) || !strings.Contains(structured.ProjectionMarkdown, value) {
			t.Fatalf("canonical role %s missing %q or absent from projection: %q", field, want, value)
		}
	}
	if strings.Contains(joinedObservations, "中心人物很醒目") || strings.Contains(structured.NextStep, "用遮挡增加前后距离") || strings.Contains(structured.NextStep, "这次没有提供") || strings.Contains(structured.ProjectionMarkdown, "画面里你最想保留的是哪一处") {
		t.Fatalf("art projection must preserve only the first complete evidenced item: %q", structured.ProjectionMarkdown)
	}
}

func TestBuildStructuredWorkFeedback_StripsProjectionMarkdownFromCanonicalFields(t *testing.T) {
	raw := `### 1. 总体评价
文章围绕爸爸帮助孩子学习展开，中心明确。

### 2. 亮点与证据
- **维度：表达**：原文中的对话让人物更真实。
- **维度：结构**：开头、中间和结尾衔接清楚。

### 3. 维度与问题
- **维度与问题：语言细节**
  - **原句**：“爸爸每天工作很忙。”
  - **建议**：补充一个爸爸陪伴孩子的具体动作。

### 4. 基础规范清单
没有发现需要家长确认的确定性字词问题。

### 5. 下一步小任务
只修改一个段落，补充一处真实互动。

### 6. 给家长的一句话
先请孩子朗读，再由孩子决定最想修改的一处。`

	d := newDataDeps(t)
	d.Solver = &fakeWorkFeedbackSolver{feedback: raw}
	id, _, err := d.CreateCreativeWork(context.Background(), "xiaoming", "s", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting,
		Title:    "我的好爸爸",
		Task:     "写一个真实片段",
		Versions: []k12.CreativeWorkVersion{{ContentMarkdown: "孩子的原稿"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := d.GenerateWorkFeedback(context.Background(), "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	feedback := view.Fields.Versions[0].StructuredFeedback
	if feedback == nil {
		t.Fatal("structured feedback missing")
	}
	if err := feedback.Validate(); err != nil {
		t.Fatalf("structured feedback should remain valid: %v", err)
	}
	if len(feedback.Observations) == 0 || len(feedback.Observations) > 3 {
		t.Fatalf("observations must remain atomic (1-3), got %#v", feedback.Observations)
	}
	for _, observation := range feedback.Observations {
		if strings.Contains(observation.Evidence, "###") ||
			strings.Contains(observation.Evidence, "**") ||
			strings.Contains(observation.Evidence, "\n") {
			t.Fatalf("canonical observation leaked projection Markdown: %q", observation.Evidence)
		}
	}
	if len(feedback.Suggestions) == 0 || len(feedback.Suggestions) > 3 {
		t.Fatalf("suggestions must remain atomic (1-3), got %#v", feedback.Suggestions)
	}
	for _, suggestion := range feedback.Suggestions {
		if strings.Contains(suggestion, "###") ||
			strings.Contains(suggestion, "**") ||
			strings.Contains(suggestion, "\n") {
			t.Fatalf("canonical suggestion leaked projection Markdown: %q", suggestion)
		}
	}
	joinedSuggestions := strings.Join(feedback.Suggestions, "\n")
	if !strings.Contains(joinedSuggestions, "补充一个爸爸陪伴孩子的具体动作") &&
		!strings.Contains(joinedSuggestions, "补充一处真实互动") {
		t.Fatalf("actionable suggestion was lost: %#v", feedback.Suggestions)
	}
	if feedback.ProjectionMarkdown == raw ||
		!strings.Contains(feedback.ProjectionMarkdown, "## 可见证据") ||
		!strings.Contains(feedback.ProjectionMarkdown, "## 先这样肯定") ||
		!strings.Contains(feedback.ProjectionMarkdown, "## 家长可以这样问或讲") ||
		!strings.Contains(feedback.ProjectionMarkdown, "## 下一次只试一个点") {
		t.Fatalf("display projection must be deterministically generated from canonical fields, got %q", feedback.ProjectionMarkdown)
	}
	assertApprovedStructuredFeedbackProjection(
		t, feedback.ProjectionMarkdown, feedback.Limitations,
	)
}

// TestGenerateWorkFeedback_Art_INV011_Rejected INV-011 拦截对美术输出同样生效：
// 视觉通道生成的点评出现打分/等第/重画口径 → 拒绝入库，作品保持 draft、不留假点评。
func TestGenerateWorkFeedback_Art_INV011_Rejected(t *testing.T) {
	cases := []struct {
		name, out string
	}{
		{"打分", "构图完整，色彩可以打 85 分。"},
		{"等第", "这幅画能评甲等。"},
		{"排名", "在同龄孩子里排名前列。"},
		{"重画", "我来重画一幅给你参考。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDataDeps(t)
			d.Solver = &fakeWorkFeedbackSolver{feedback: tc.out}
			ctx := context.Background()
			id, _, err := d.CreateCreativeWork(ctx, "xiaoming", "s", k12.CreativeWorkFields{
				WorkType: k12.WorkTypeArt, Title: "《雨后的校园》", Task: "画一处雨后场景",
				Versions: []k12.CreativeWorkVersion{{SourceAssetID: "asset-1"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
				t.Fatalf("美术输出 %q 违反 INV-011，应拒绝", tc.out)
			}
			v, _ := d.GetCreativeWork(ctx, "xiaoming", id)
			if v.Record.Status != k12.WorkStatusDraft ||
				v.Fields.Versions[0].Feedback != "" || v.Fields.Versions[0].FeedbackSource != "" {
				t.Fatalf("拒绝后不得残留点评: status=%s %+v", v.Record.Status, v.Fields.Versions[0])
			}
		})
	}
}

// TestGenerateWorkFeedback_OwnerIsolation 归属隔离：别的实例不能给他人作品生成点评。
func TestGenerateWorkFeedback_OwnerIsolation(t *testing.T) {
	d := newDataDeps(t, "xiaoming", "xiaohong")
	gen := &fakeWorkFeedbackSolver{feedback: "好句在开头；建议结尾具体化。"}
	d.Solver = gen
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	if _, err := d.GenerateWorkFeedback(ctx, "xiaohong", id); err == nil {
		t.Fatal("跨实例生成点评应被拒")
	}
	if gen.calls != 0 {
		t.Fatal("归属校验必须在调用 Skill 之前，不应触发生成")
	}
}

// TestGenerateWorkFeedback_StatusGuard 旧生成入口受状态限制；修改稿入口只读拒绝，
// 新 command key 通过当前命令入口追加新的点评 generation。
func TestGenerateWorkFeedback_StatusGuard(t *testing.T) {
	d := newDataDeps(t)
	gen := &fakeWorkFeedbackSolver{feedback: "好句：开头比喻；建议：结尾补细节。"}
	d.Solver = gen
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")

	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err != nil {
		t.Fatalf("draft 生成: %v", err)
	}
	// 旧入口已是 feedback_ready 时不得隐式追加 generation。
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
		t.Fatal("feedback_ready 状态不应可再次生成点评")
	}
	beforeRevision, err := d.GetCreativeWork(ctx, "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if beforeRevision.GenerationState.Initial == nil || beforeRevision.GenerationState.Latest == nil {
		t.Fatalf("首轮点评应建立初始/latest generation: %+v", beforeRevision.GenerationState)
	}
	beforeInitialID := beforeRevision.GenerationState.Initial.GenerationID
	beforeLatestID := beforeRevision.GenerationState.Latest.GenerationID
	beforeRowVersion := beforeRevision.GenerationState.RowVersion
	beforeRecordVersion := beforeRevision.Record.Version
	beforeVersionCount := len(beforeRevision.Fields.Versions)
	beforeCalls := gen.calls

	// 修改稿入口必须 fail-closed，拒绝后不得追加 generation/version。
	if _, err := d.SubmitRevision(ctx, "xiaoming", id, "柳枝像绿色的丝带，风一吹沙沙响。", ""); err == nil {
		t.Fatal("当前作品不应允许提交修改稿")
	}
	afterRevision, err := d.GetCreativeWork(ctx, "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRevision.Fields.Versions) != beforeVersionCount ||
		afterRevision.Record.Version != beforeRecordVersion ||
		afterRevision.GenerationState.RowVersion != beforeRowVersion ||
		afterRevision.GenerationState.Initial == nil ||
		afterRevision.GenerationState.Initial.GenerationID != beforeInitialID ||
		afterRevision.GenerationState.Latest == nil ||
		afterRevision.GenerationState.Latest.GenerationID != beforeLatestID ||
		gen.calls != beforeCalls {
		t.Fatalf("修改稿拒绝后不得产生任何副作用: before=%+v after=%+v calls=%d->%d",
			beforeRevision, afterRevision, beforeCalls, gen.calls)
	}

	const regenerateCommandKey = "feedback-status-guard-regenerate"
	regenerated, err := d.GenerateWorkFeedbackCommand(
		ctx, "xiaoming", id, regenerateCommandKey,
	)
	if err != nil {
		t.Fatalf("新 command key 应可追加一轮点评: %v", err)
	}
	if regenerated.GenerationState.Initial == nil ||
		regenerated.GenerationState.Initial.GenerationID != beforeInitialID ||
		regenerated.GenerationState.Latest == nil ||
		regenerated.GenerationState.Latest.GenerationID == beforeLatestID ||
		regenerated.GenerationState.Latest.GenerationNo != 2 ||
		regenerated.GenerationState.Latest.CommandKey != regenerateCommandKey ||
		len(regenerated.Fields.Versions) != beforeVersionCount {
		t.Fatalf("新 command key 应只追加 generation，不追加 legacy version: %+v",
			regenerated)
	}
	// archived 拒。
	id2 := newWritingWork(t, d, "xiaoming")
	forceCreativeWorkStatus(t, d, id2, k12.WorkStatusArchived)
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id2); err == nil {
		t.Fatal("archived 不应可生成点评")
	}
}

// TestGenerateWorkFeedback_ExecutorFailureHonest 生成失败：诚实报错，不留假点评。
func TestGenerateWorkFeedback_ExecutorFailureHonest(t *testing.T) {
	d := newDataDeps(t)
	d.Solver = &fakeWorkFeedbackSolver{err: errors.New("provider down")}
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
		t.Fatal("生成失败应报错")
	}
	v, _ := d.GetCreativeWork(ctx, "xiaoming", id)
	if v.Record.Status != k12.WorkStatusDraft || v.Fields.Versions[0].Feedback != "" {
		t.Fatalf("失败后不得留下假点评: status=%s %+v", v.Record.Status, v.Fields.Versions[0])
	}
}

// TestGenerateWorkFeedback_GeneratorUnconfigured 未接线生成能力 → 诚实报错。
func TestGenerateWorkFeedback_GeneratorUnconfigured(t *testing.T) {
	d := newDataDeps(t) // Solver 为 nil
	ctx := context.Background()
	id := newWritingWork(t, d, "xiaoming")
	if _, err := d.GenerateWorkFeedback(ctx, "xiaoming", id); err == nil {
		t.Fatal("未配置点评生成能力应报错")
	}
}
