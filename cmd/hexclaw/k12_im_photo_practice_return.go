package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

var k12InboundPhotoPaperSeqPattern = regexp.MustCompile(`^\s*([0-9]+)(?:\s*[.)、:：]|\s*$)`)

type k12InboundPhotoPracticeUsecases interface {
	ListPracticeSets(context.Context, string, string) ([]k12usecase.PracticeSetView, error)
	GetPracticeSet(context.Context, string, string) (k12usecase.PracticeSetView, error)
	SubmitReturn(
		context.Context, string, string, string, string, []string,
	) (k12usecase.PracticeSetView, error)
}

type k12InboundPhotoPracticeRegrader interface {
	Process(context.Context, string, string, string) error
}

type k12InboundPhotoPracticeArtifactReader interface {
	GetCurrentGradingFinalArtifactByJob(context.Context, string, string) (k12.GradingFinalArtifact, error)
}

type k12DingtalkPracticeReturnBridge struct {
	practice  k12InboundPhotoPracticeUsecases
	regrader  k12InboundPhotoPracticeRegrader
	artifacts k12InboundPhotoPracticeArtifactReader
}

func newK12DingtalkPracticeReturnBridge(
	practice k12InboundPhotoPracticeUsecases,
	regrader k12InboundPhotoPracticeRegrader,
	artifacts k12InboundPhotoPracticeArtifactReader,
) *k12DingtalkPracticeReturnBridge {
	return &k12DingtalkPracticeReturnBridge{
		practice: practice, regrader: regrader, artifacts: artifacts,
	}
}

func (b *k12DingtalkPracticeReturnBridge) ListPracticeSets(
	ctx context.Context,
	agentName string,
) ([]k12usecase.PracticeSetView, error) {
	if b == nil || b.practice == nil {
		return nil, fmt.Errorf("DingTalk practice-return reader is unavailable")
	}
	return b.practice.ListPracticeSets(ctx, agentName, "")
}

func k12DingtalkInboundReturnID(receiptID string) (string, error) {
	receiptID = strings.TrimSpace(receiptID)
	if receiptID == "" {
		return "", fmt.Errorf("DingTalk inbound receipt ID is required")
	}
	return "dingtalk-inbound:" + receiptID, nil
}

func (b *k12DingtalkPracticeReturnBridge) ResumePracticeReturn(
	ctx context.Context,
	agentName, receiptID string,
) (k12InboundPhotoPracticeReturnState, error) {
	if b == nil || b.practice == nil {
		return k12InboundPhotoPracticeReturnState{},
			fmt.Errorf("DingTalk practice-return runtime is unavailable")
	}
	returnID, err := k12DingtalkInboundReturnID(receiptID)
	if err != nil {
		return k12InboundPhotoPracticeReturnState{}, err
	}
	sets, err := b.practice.ListPracticeSets(ctx, agentName, "")
	if err != nil {
		return k12InboundPhotoPracticeReturnState{}, err
	}
	var matched *k12usecase.PracticeSetView
	for index := range sets {
		for _, returned := range sets[index].Fields.ReturnAssets {
			if returned.ReturnID != returnID {
				continue
			}
			if matched != nil {
				return k12InboundPhotoPracticeReturnState{},
					fmt.Errorf("DingTalk inbound receipt binds multiple practice sets")
			}
			copy := sets[index]
			matched = &copy
			break
		}
	}
	if matched == nil {
		return k12InboundPhotoPracticeReturnState{}, records.ErrNotFound
	}
	return b.advanceBoundPracticeReturn(ctx, agentName, *matched, returnID)
}

func (b *k12DingtalkPracticeReturnBridge) AdvancePracticeReturn(
	ctx context.Context,
	input k12InboundPhotoPracticeReturnInput,
) (k12InboundPhotoPracticeReturnState, error) {
	if existing, err := b.ResumePracticeReturn(ctx, input.AgentName, input.ReceiptID); err == nil {
		return existing, nil
	} else if !errors.Is(err, records.ErrNotFound) {
		return k12InboundPhotoPracticeReturnState{}, err
	}
	if b.regrader == nil || b.artifacts == nil {
		return k12InboundPhotoPracticeReturnState{},
			fmt.Errorf("DingTalk practice-return processing dependencies are incomplete")
	}
	set, err := b.practice.GetPracticeSet(ctx, input.AgentName, input.PracticeSetID)
	if err != nil {
		return k12InboundPhotoPracticeReturnState{}, err
	}
	itemIDs, ok := k12InboundPhotoPracticeItemIDs(set, input.Questions)
	if !ok {
		return k12InboundPhotoPracticeReturnState{},
			fmt.Errorf("DingTalk practice-return item alignment is unresolved")
	}
	returnID, err := k12DingtalkInboundReturnID(input.ReceiptID)
	if err != nil {
		return k12InboundPhotoPracticeReturnState{}, err
	}
	set, err = b.practice.SubmitReturn(
		ctx, input.AgentName, input.PracticeSetID, returnID, input.AssetID, itemIDs,
	)
	if err != nil {
		return k12InboundPhotoPracticeReturnState{}, err
	}
	return b.advanceBoundPracticeReturn(ctx, input.AgentName, set, returnID)
}

func (b *k12DingtalkPracticeReturnBridge) advanceBoundPracticeReturn(
	ctx context.Context,
	agentName string,
	set k12usecase.PracticeSetView,
	returnID string,
) (k12InboundPhotoPracticeReturnState, error) {
	if set.Record == nil || strings.TrimSpace(set.Record.RecordID) == "" {
		return k12InboundPhotoPracticeReturnState{},
			fmt.Errorf("DingTalk practice-return set identity is invalid")
	}
	returned, ok := k12InboundPhotoReturnAsset(set, returnID)
	if !ok {
		return k12InboundPhotoPracticeReturnState{}, records.ErrNotFound
	}
	switch returned.RegradeStatus {
	case k12.PracticeRegradeCompleted, k12.PracticeRegradeNeedsReview:
	case k12.PracticeRegradeFailedTerminal, k12.PracticeRegradeOutcomeUnknown:
		return k12InboundPhotoPracticeReturnState{},
			fmt.Errorf("DingTalk practice-return regrade is not safely retryable")
	default:
		if b.regrader == nil {
			return k12InboundPhotoPracticeReturnState{},
				fmt.Errorf("DingTalk practice-return regrader is unavailable")
		}
		if err := b.regrader.Process(
			ctx, agentName, set.Record.RecordID, returnID,
		); err != nil {
			return k12InboundPhotoPracticeReturnState{}, err
		}
		latest, err := b.practice.GetPracticeSet(ctx, agentName, set.Record.RecordID)
		if err != nil {
			return k12InboundPhotoPracticeReturnState{}, err
		}
		set = latest
		returned, ok = k12InboundPhotoReturnAsset(set, returnID)
		if !ok {
			return k12InboundPhotoPracticeReturnState{}, records.ErrNotFound
		}
	}
	state := k12InboundPhotoPracticeReturnState{
		PracticeSetID: set.Record.RecordID,
		ReturnID:      returnID,
	}
	if strings.TrimSpace(returned.RegradeJobID) == "" || b.artifacts == nil {
		return state, nil
	}
	artifact, err := b.artifacts.GetCurrentGradingFinalArtifactByJob(
		ctx, agentName, returned.RegradeJobID,
	)
	if errors.Is(err, records.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return k12InboundPhotoPracticeReturnState{}, err
	}
	state.FinalArtifactID = artifact.ArtifactID
	return state, nil
}

func k12InboundPhotoReturnAsset(
	set k12usecase.PracticeSetView,
	returnID string,
) (k12.PracticeReturnAsset, bool) {
	for _, returned := range set.Fields.ReturnAssets {
		if returned.ReturnID == returnID {
			return returned, true
		}
	}
	return k12.PracticeReturnAsset{}, false
}

// k12InboundPhotoPracticeItemIDs 只采用冻结卷面题号；全部题号缺失时，
// 仅在作答块数与整卷可发布题数完全一致时允许按顺序对齐。
func k12InboundPhotoPracticeItemIDs(
	set k12usecase.PracticeSetView,
	questions []k12usecase.RecognizedQuestion,
) ([]string, bool) {
	items := make([]k12.PracticeItem, 0, len(set.Fields.Items))
	for _, item := range set.Fields.Items {
		if k12.PracticeItemPublishable(item) && item.PaperSeq > 0 {
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].PaperSeq < items[j].PaperSeq })
	answerable := k12usecase.RecognizedQuestionsForAssessment(questions)
	if len(items) == 0 || len(answerable) == 0 {
		return nil, false
	}
	bySeq := make(map[int]string, len(items))
	for _, item := range items {
		if _, duplicate := bySeq[item.PaperSeq]; duplicate {
			return nil, false
		}
		bySeq[item.PaperSeq] = item.ItemID
	}
	matched := make(map[int]struct{}, len(answerable))
	missingNumber := 0
	for _, question := range answerable {
		seq, ok := k12InboundPhotoRecognizedPaperSeq(question)
		if !ok {
			missingNumber++
			continue
		}
		if _, exists := bySeq[seq]; !exists {
			return nil, false
		}
		matched[seq] = struct{}{}
	}
	if missingNumber == len(answerable) {
		if len(answerable) != len(items) {
			return nil, false
		}
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ItemID)
		}
		return ids, true
	}
	if missingNumber != 0 || len(matched) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(matched))
	for _, item := range items {
		if _, exists := matched[item.PaperSeq]; exists {
			ids = append(ids, item.ItemID)
		}
	}
	return ids, len(ids) > 0
}

func k12InboundPhotoRecognizedPaperSeq(
	question k12usecase.RecognizedQuestion,
) (int, bool) {
	candidates := append([]string(nil), question.SourceNumberPath...)
	candidates = append(candidates, question.DisplayLabel)
	for _, candidate := range candidates {
		match := k12InboundPhotoPaperSeqPattern.FindStringSubmatch(candidate)
		if len(match) != 2 {
			continue
		}
		seq, err := strconv.Atoi(match[1])
		if err == nil && seq > 0 {
			return seq, true
		}
	}
	return 0, false
}
