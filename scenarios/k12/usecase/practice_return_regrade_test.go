package usecase

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

const practiceReturnOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

type practiceReturnGradingFake struct {
	started StartPhotoGradingInput
	job     GradingJobView
	result  PhotoGradeResult
}

func (f *practiceReturnGradingFake) StartPhotoGradingJob(
	_ context.Context,
	in StartPhotoGradingInput,
) (GradingJobView, bool, error) {
	f.started = in
	return f.job, true, nil
}

func (f *practiceReturnGradingFake) RunGradingJob(
	context.Context,
	string,
) (GradingJobView, error) {
	return f.job, nil
}

func (f *practiceReturnGradingFake) PhotoResult(string) (PhotoGradeResult, bool) {
	return f.result, true
}

func (f *practiceReturnGradingFake) RecognizedQuestionsForOwner(
	context.Context,
	string,
	string,
) ([]RecognizedQuestion, bool) {
	return nil, false
}

func TestPracticeReturnRegradeCoordinator_AppliesClearResultsAndPersistsAnnotatedGuide(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	setID := seedRegradePaper(t, d, "")
	set, err := d.GetPracticeSet(context.Background(), "mingming", setID)
	if err != nil {
		t.Fatal(err)
	}
	ret := set.Fields.ReturnAssets[0]
	route := k12.GradingModelSnapshot{
		Provider: "provider-a",
		Model:    "vision-a",
		Route:    "provider-a/vision-a",
	}
	annotated, err := base64.StdEncoding.DecodeString(practiceReturnOnePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	grading := &practiceReturnGradingFake{
		job: GradingJobView{
			Record: &records.AgentRecord{
				RecordID: "grade-return-1",
				Status:   k12.GradingStageCompleted,
			},
			Fields: k12.GradingJobFields{ModelSnapshot: route},
		},
		result: PhotoGradeResult{
			Mode:          PhotoModeGrade,
			TaskIntent:    PhotoTaskCompletedHomework,
			ResultSurface: PhotoSurfaceAnnotatedHomework,
			Items: []PhotoGradeItem{
				{Status: PhotoCorrect},
				{
					Status: PhotoWrong,
					ParentGuide: &ParentTeachingGuide{
						Answer:           "1.82",
						GradeLevelMethod: "先把小数乘法看成整数乘法，再点小数点。",
					},
				},
			},
			AnnotatedImage: &RenderedPhoto{Data: annotated, MIME: "image/png"},
			Markdown:       "## 批改结果\n\n第 2 题需要讲解。",
		},
	}
	coordinator := &PracticeReturnRegradeCoordinator{
		Deps:    &d,
		Grading: grading,
	}

	if err := coordinator.Process(context.Background(), "mingming", setID, ret.ReturnID); err != nil {
		t.Fatalf("自动复批: %v", err)
	}
	got, err := d.GetPracticeSet(context.Background(), "mingming", setID)
	if err != nil {
		t.Fatal(err)
	}
	projected := got.Fields.ReturnAssets[0]
	if projected.RegradeJobID != "grade-return-1" ||
		projected.RegradeStatus != k12.PracticeRegradeCompleted ||
		projected.RouteSnapshot != route ||
		projected.AnnotatedAssetID == "" ||
		projected.ResultMarkdown != grading.result.Markdown ||
		len(projected.UnresolvedItemIDs) != 0 {
		t.Fatalf("自动复批投影不完整: %+v", projected)
	}
	if owner, ok := assetstore.OwnerOf(projected.AnnotatedAssetID); !ok || owner != "mingming" {
		t.Fatalf("批注图未按 owner 内容寻址保存: %q", projected.AnnotatedAssetID)
	}
	if grading.started.SourceKind != PracticeReturnGradingSourceKind ||
		grading.started.SourceKey != setID+":"+ret.ReturnID ||
		grading.started.Photo.TaskIntent != PhotoTaskCompletedHomework {
		t.Fatalf("复批任务没有冻结为练习回传语义: %+v", grading.started)
	}
	for index, item := range got.Fields.Items {
		if item.ResultCorrect == nil ||
			*item.ResultCorrect != (index == 0) ||
			item.ResultEvidence != k12.PracticeResultSystemVerified {
			t.Fatalf("题 %d 自动结论/证据错误: %+v", index, item)
		}
	}
}

func TestPracticeReturnRegradeCoordinator_OnlyProjectsTrueUncertainty(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	setID := seedRegradePaper(t, d, "")
	set, err := d.GetPracticeSet(context.Background(), "mingming", setID)
	if err != nil {
		t.Fatal(err)
	}
	ret := set.Fields.ReturnAssets[0]
	grading := &practiceReturnGradingFake{
		job: GradingJobView{
			Record: &records.AgentRecord{
				RecordID: "grade-return-review",
				Status:   k12.GradingStageCompleted,
			},
			Fields: k12.GradingJobFields{ModelSnapshot: k12.GradingModelSnapshot{
				Provider: "p", Model: "m", Route: "p/m",
			}},
		},
		result: PhotoGradeResult{
			Items: []PhotoGradeItem{
				{Status: PhotoCorrect},
				{Status: PhotoUntrusted},
			},
			Markdown: "第 2 题看不清，需要家长核对。",
		},
	}
	coordinator := &PracticeReturnRegradeCoordinator{Deps: &d, Grading: grading}

	if err := coordinator.Process(context.Background(), "mingming", setID, ret.ReturnID); err != nil {
		t.Fatalf("自动复批: %v", err)
	}
	got, err := d.GetPracticeSet(context.Background(), "mingming", setID)
	if err != nil {
		t.Fatal(err)
	}
	projected := got.Fields.ReturnAssets[0]
	if projected.RegradeStatus != k12.PracticeRegradeNeedsReview ||
		len(projected.UnresolvedItemIDs) != 1 ||
		projected.UnresolvedItemIDs[0] != got.Fields.Items[1].ItemID {
		t.Fatalf("只应降级真实不确定题: %+v", projected)
	}
	if got.Fields.Items[0].ResultCorrect == nil || !*got.Fields.Items[0].ResultCorrect {
		t.Fatalf("清晰题不应被同批不确定题阻断: %+v", got.Fields.Items[0])
	}
	if got.Fields.Items[1].ResultCorrect != nil {
		t.Fatalf("不确定题不得猜测结论: %+v", got.Fields.Items[1])
	}
}
