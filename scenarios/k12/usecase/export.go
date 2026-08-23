package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type LearningArchiveExportV1 = k12.LearningArchiveExportV1
type LearningArchiveScope = k12.LearningArchiveScope
type LearningArchiveObjectCounts = k12.LearningArchiveObjectCounts

const learningArchiveDigestDomain = "hexclaw:k12:learning-archive:v1"

// MistakeSheetMarkdown 生成「错题卷」：把到期该练的错题排成可打印卷子（只出题、不给答案，留重做空间）。
//
// 这是 M2-2 周五错题卷 / 「一键出错题卷」的产物内容；由 cron 投递或前端打印（§3.5.4）。
func (d Deps) MistakeSheetMarkdown(ctx context.Context, agentName string) (string, error) {
	items, err := d.ReviewQueue(ctx, agentName)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 本周错题卷\n\n共 %d 题，做完对照错题本订正。\n\n", len(items))
	for i, it := range items {
		// BUG-20260710：队列跨集合混排（错题本+积累本），积累项的 Fields.Question 为零值——
		// 必须用跨集合安全的 Title()（错题项仍返回 Fields.Question，语义不变）。
		fmt.Fprintf(&b, "**%d.** %s\n\n（　　）\n\n", i+1, it.Title())
	}
	if len(items) == 0 {
		b.WriteString("本周没有到期该练的错题，继续保持。\n")
	}
	return b.String(), nil
}

// ExportMistakesMarkdown 把某实例的错题本导出为 Markdown 文本。
//
// 只产内容（结构化 Markdown）；PDF/Word 渲染由平台 render 服务承接（§3.8：PDF 复用错题卷版式 / Word 供老师批注）。
func (d Deps) ExportMistakesMarkdown(ctx context.Context, agentName string) (string, error) {
	if agentName == "" {
		return "", fmt.Errorf("usecase: agentName 不可空")
	}
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		return "", fmt.Errorf("usecase: 导出错题本: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 错题本导出\n\n共 %d 题\n\n", len(recs))
	for i, r := range recs {
		f, _ := k12.ParseMistakeFields(r.Fields)
		fmt.Fprintf(&b, "## %d. %s\n\n", i+1, f.Question)
		if f.KnowledgePoint != "" {
			fmt.Fprintf(&b, "- 知识点：%s\n", f.KnowledgePoint)
		}
		if f.ErrorCause != "" {
			fmt.Fprintf(&b, "- 错因：%s\n", f.ErrorCause)
		}
		if f.WrongProcess != "" {
			fmt.Fprintf(&b, "- 错误过程：%s\n", f.WrongProcess)
		}
		fmt.Fprintf(&b, "- 状态：%s\n\n", r.Status)
	}
	return b.String(), nil
}

// ExportLearningArchiveMarkdown 从一个只读快照生成五对象 canonical Markdown，
// 并把相同来源收敛到同一个不可变 Artifact。
func (d Deps) ExportLearningArchiveMarkdown(
	ctx context.Context,
	agentName string,
) (LearningArchiveExportV1, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return LearningArchiveExportV1{}, fmt.Errorf("usecase: learning archive agent is required")
	}
	if d.Records == nil {
		return LearningArchiveExportV1{}, fmt.Errorf("usecase: learning archive store is not configured")
	}

	snapshot, err := d.Records.ReadLearningArchiveSourceSnapshot(ctx, agentName, d.now())
	if err != nil {
		return LearningArchiveExportV1{}, fmt.Errorf("usecase: read learning archive snapshot: %w", err)
	}
	if err := validateLearningArchiveWorks(snapshot.CreativeWorks); err != nil {
		return LearningArchiveExportV1{}, err
	}
	markdown, err := renderLearningArchiveMarkdown(snapshot)
	if err != nil {
		return LearningArchiveExportV1{}, err
	}
	counts := LearningArchiveObjectCounts{
		WeeklyReview:  len(snapshot.WeeklyReview),
		Mistakes:      len(snapshot.Mistakes),
		PracticeSets:  len(snapshot.PracticeSets),
		Accumulation:  len(snapshot.Accumulations),
		CreativeWorks: len(snapshot.CreativeWorks),
	}
	scope := LearningArchiveScope{Agent: agentName, GradeTerm: snapshot.Profile.GradeTerm}
	digest, err := learningArchiveSourceDigest(scope, counts, snapshot)
	if err != nil {
		return LearningArchiveExportV1{}, err
	}
	artifactID := learningArchiveArtifactID(scope, digest)
	stored, _, err := d.Records.FreezeLearningArchiveArtifact(ctx, k12.PrintArtifact{
		ArtifactID: artifactID, AgentName: agentName,
		SourceKind: k12.PrintSourceLearningArchive,
		SourceRef:  "grade-term:" + snapshot.Profile.GradeTerm,
		Title:      "学习档案", CanonicalMarkdown: markdown,
		SourceDigest: digest, CreatedAt: snapshot.AsOf,
	})
	if err != nil {
		return LearningArchiveExportV1{}, fmt.Errorf("usecase: freeze learning archive artifact: %w", err)
	}
	return LearningArchiveExportV1{
		SchemaVersion: k12.LearningArchiveSchemaVersion,
		Scope:         scope, AsOf: stored.CreatedAt, SourceDigest: stored.SourceDigest,
		ObjectCounts: counts, ArtifactID: stored.ArtifactID,
		CanonicalMarkdown: stored.CanonicalMarkdown,
	}, nil
}

type learningArchiveRecordDigest struct {
	RecordID string `json:"record_id"`
	Status   string `json:"status"`
	DueAt    *int64 `json:"due_at,omitempty"`
	Fields   string `json:"fields_json"`
}

type learningArchiveWorkDigest struct {
	Record  learningArchiveRecordDigest      `json:"record"`
	Initial learningArchiveGenerationDigest  `json:"initial_generation"`
	Latest  *learningArchiveGenerationDigest `json:"latest_generation,omitempty"`
}

type learningArchiveGenerationDigest struct {
	GenerationID string                         `json:"generation_id"`
	WorkID       string                         `json:"work_id"`
	Status       string                         `json:"status"`
	FeedbackType string                         `json:"feedback_type"`
	Source       k12.CreativeWorkSourceSnapshot `json:"source"`
	Feedback     *k12.WorkFeedback              `json:"feedback,omitempty"`
}

func learningArchiveSourceDigest(
	scope LearningArchiveScope,
	counts LearningArchiveObjectCounts,
	snapshot k12storage.LearningArchiveSourceSnapshot,
) (string, error) {
	canonical := struct {
		SchemaVersion string                        `json:"schema_version"`
		Scope         LearningArchiveScope          `json:"scope"`
		ObjectCounts  LearningArchiveObjectCounts   `json:"object_counts"`
		WeeklyReview  []k12.WeeklyPracticeItem      `json:"weekly_review"`
		Mistakes      []learningArchiveRecordDigest `json:"mistakes"`
		PracticeSets  []learningArchiveRecordDigest `json:"practice_sets"`
		Accumulation  []learningArchiveRecordDigest `json:"accumulation"`
		CreativeWorks []learningArchiveWorkDigest   `json:"creative_works"`
	}{
		SchemaVersion: k12.LearningArchiveSchemaVersion,
		Scope:         scope, ObjectCounts: counts, WeeklyReview: snapshot.WeeklyReview,
		Mistakes:      learningArchiveRecordDigests(snapshot.Mistakes),
		PracticeSets:  learningArchiveRecordDigests(snapshot.PracticeSets),
		Accumulation:  learningArchiveRecordDigests(snapshot.Accumulations),
		CreativeWorks: make([]learningArchiveWorkDigest, 0, len(snapshot.CreativeWorks)),
	}
	for _, work := range snapshot.CreativeWorks {
		initial := learningArchiveGenerationSourceDigest(*work.Initial)
		var latest *learningArchiveGenerationDigest
		if work.Latest != nil {
			value := learningArchiveGenerationSourceDigest(*work.Latest)
			latest = &value
		}
		canonical.CreativeWorks = append(canonical.CreativeWorks, learningArchiveWorkDigest{
			Record: learningArchiveRecordDigest{
				RecordID: work.Record.RecordID, Status: work.Record.Status,
				DueAt: work.Record.DueAt, Fields: work.Record.Fields,
			},
			Initial: initial, Latest: latest,
		})
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("usecase: encode learning archive source: %w", err)
	}
	return digestLearningArchiveCanonicalJSON(raw), nil
}

func digestLearningArchiveCanonicalJSON(raw []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(learningArchiveDigestDomain + "\x00"))
	_, _ = hash.Write(raw)
	return hex.EncodeToString(hash.Sum(nil))
}

func learningArchiveGenerationSourceDigest(source k12.WorkFeedbackGeneration) learningArchiveGenerationDigest {
	return learningArchiveGenerationDigest{
		GenerationID: source.GenerationID,
		WorkID:       source.WorkID,
		Status:       source.Status,
		FeedbackType: source.FeedbackType,
		Source:       source.Source,
		Feedback:     source.Feedback,
	}
}

func learningArchiveRecordDigests(source []*records.AgentRecord) []learningArchiveRecordDigest {
	out := make([]learningArchiveRecordDigest, 0, len(source))
	for _, record := range source {
		out = append(out, learningArchiveRecordDigest{
			RecordID: record.RecordID, Status: record.Status,
			DueAt: record.DueAt, Fields: record.Fields,
		})
	}
	return out
}

func learningArchiveArtifactID(scope LearningArchiveScope, digest string) string {
	sum := sha256.Sum256([]byte(scope.Agent + "\x00" + scope.GradeTerm + "\x00" + digest))
	return "learning-archive-" + hex.EncodeToString(sum[:16])
}

func validateLearningArchiveWorks(works []k12storage.LearningArchiveCreativeWork) error {
	for _, work := range works {
		if work.Record == nil || work.Initial == nil {
			return fmt.Errorf("usecase: learning archive creative work is missing its initial source")
		}
		initial := work.Initial
		if initial.WorkID != work.Record.RecordID || initial.AgentName != work.Record.AgentName ||
			initial.Source.WorkType != work.Fields.WorkType {
			return fmt.Errorf("usecase: learning archive creative work source identity is invalid")
		}
		if initial.Source.WorkType == k12.WorkTypeWriting &&
			initial.Source.ContentMarkdown == "" && initial.Source.SourceAssetID == "" {
			return fmt.Errorf("usecase: learning archive writing source is empty")
		}
		if initial.Source.WorkType == k12.WorkTypeArt && initial.Source.SourceAssetID == "" {
			return fmt.Errorf("usecase: learning archive art source is empty")
		}
		if work.Latest != nil &&
			(work.Latest.WorkID != work.Record.RecordID || work.Latest.AgentName != work.Record.AgentName) {
			return fmt.Errorf("usecase: learning archive latest feedback identity is invalid")
		}
	}
	return nil
}

func renderLearningArchiveMarkdown(
	snapshot k12storage.LearningArchiveSourceSnapshot,
) (string, error) {
	var b strings.Builder
	b.WriteString("# 学习档案\n\n")
	b.WriteString("## 本周该练\n\n")
	fmt.Fprintf(&b, "共 %d 项\n\n", len(snapshot.WeeklyReview))
	for i, item := range snapshot.WeeklyReview {
		fmt.Fprintf(&b, "### %d\n\n", i+1)
		writeLearningArchiveBlock(&b, item.PromptMarkdown)
		writeLearningArchiveMeta(&b, "学科", item.Subject)
		writeLearningArchiveMeta(&b, "知识点", item.KnowledgePoint)
		b.WriteByte('\n')
	}

	b.WriteString("## 全部错题\n\n")
	fmt.Fprintf(&b, "共 %d 题\n\n", len(snapshot.Mistakes))
	for i, record := range snapshot.Mistakes {
		fields, err := k12.ParseMistakeFields(record.Fields)
		if err != nil {
			return "", fmt.Errorf("usecase: parse learning archive mistake: %w", err)
		}
		fmt.Fprintf(&b, "### %d\n\n", i+1)
		writeLearningArchiveBlock(&b, fields.Question)
		writeLearningArchiveMeta(&b, "知识点", fields.KnowledgePoint)
		writeLearningArchiveMeta(&b, "错因", fields.ErrorCause)
		writeLearningArchiveLabeledBlock(&b, "规范答案", fields.CanonicalAnswer)
		writeLearningArchiveLabeledBlock(&b, "错误过程", fields.WrongProcess)
		writeLearningArchiveMeta(&b, "状态", record.Status)
		b.WriteByte('\n')
	}

	b.WriteString("## 练习集\n\n")
	fmt.Fprintf(&b, "共 %d 组\n\n", len(snapshot.PracticeSets))
	for i, record := range snapshot.PracticeSets {
		fields, err := k12.ParsePracticeSetFields(record.Fields)
		if err != nil {
			return "", fmt.Errorf("usecase: parse learning archive practice set: %w", err)
		}
		fmt.Fprintf(&b, "### %d\n\n", i+1)
		writeLearningArchiveMeta(&b, "标题", fields.Title)
		for _, item := range fields.Items {
			writeLearningArchiveBlock(&b, item.QuestionMarkdown)
			writeLearningArchiveLabeledBlock(&b, "答案", item.ExpectedAnswerMarkdown)
		}
		writeLearningArchiveMeta(&b, "状态", record.Status)
		b.WriteByte('\n')
	}

	b.WriteString("## 积累\n\n")
	fmt.Fprintf(&b, "共 %d 条\n\n", len(snapshot.Accumulations))
	for i, record := range snapshot.Accumulations {
		fields, err := k12.ParseAccumFields(record.Fields)
		if err != nil {
			return "", fmt.Errorf("usecase: parse learning archive accumulation: %w", err)
		}
		fmt.Fprintf(&b, "### %d\n\n", i+1)
		writeLearningArchiveBlock(&b, fields.Content)
		writeLearningArchiveMeta(&b, "学科", fields.Subject)
		writeLearningArchiveMeta(&b, "类型", fields.EntryType)
		writeLearningArchiveMeta(&b, "来源", fields.Source)
		writeLearningArchiveMeta(&b, "状态", record.Status)
		b.WriteByte('\n')
	}

	b.WriteString("## 作品\n\n")
	fmt.Fprintf(&b, "共 %d 件\n\n", len(snapshot.CreativeWorks))
	for i, work := range snapshot.CreativeWorks {
		fmt.Fprintf(&b, "### %d\n\n", i+1)
		writeLearningArchiveMeta(&b, "标题", work.Fields.WorkTitle)
		writeLearningArchiveBlock(&b, work.Initial.Source.ContentMarkdown)
		writeLearningArchiveMeta(&b, "源文件", work.Initial.Source.SourceAssetID)
		if work.Latest != nil && work.Latest.Feedback != nil {
			writeLearningArchiveBlock(&b, work.Latest.Feedback.ProjectionMarkdown)
		}
		writeLearningArchiveMeta(&b, "状态", work.Record.Status)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func writeLearningArchiveLabeledBlock(b *strings.Builder, label, value string) {
	if value == "" {
		return
	}
	b.WriteString(label)
	b.WriteString("\n\n")
	writeLearningArchiveBlock(b, value)
}

func writeLearningArchiveBlock(b *strings.Builder, value string) {
	if value == "" {
		return
	}
	b.WriteString(value)
	if !strings.HasSuffix(value, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
}

func writeLearningArchiveMeta(b *strings.Builder, label, value string) {
	if value != "" {
		fmt.Fprintf(b, "- %s：%s\n", label, escapeLearningArchiveMeta(value))
	}
}

func escapeLearningArchiveMeta(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '\r' || r == '\n' || r == '\t' || r == '\u2028' || r == '\u2029' || r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		case strings.ContainsRune(`\\`+"`*_{}[]()<>#+-!|>~:/", r):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
