package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

const PracticeReturnGradingSourceKind = "practice_return"

// PracticeReturnGrading is the narrow internal seam used by automatic
// practice-return regrading. The public client never addresses a GradingJob.
type PracticeReturnGrading interface {
	StartPhotoGradingJob(context.Context, StartPhotoGradingInput) (GradingJobView, bool, error)
	RunGradingJob(context.Context, string) (GradingJobView, error)
	PhotoResult(string) (PhotoGradeResult, bool)
	RecognizedQuestionsForOwner(context.Context, string, string) ([]RecognizedQuestion, bool)
}

// PracticeReturnRegradeCoordinator joins immutable PracticeReturnAsset evidence
// to the existing durable GradingJob. SQLite/PracticeSet remains the source of
// truth; the in-process map only prevents concurrent duplicate workers.
type PracticeReturnRegradeCoordinator struct {
	Deps        *Deps
	Grading     PracticeReturnGrading
	BaseContext context.Context

	mu          sync.Mutex
	active      map[string]bool
	sealed      bool
	workerCount int
	workerIdle  chan struct{}
	runCtx      context.Context
	runCancel   context.CancelFunc
}

var ErrPracticeReturnRegradeShutdown = errors.New("practice return regrade coordinator is shut down")

type practiceReturnRegradeProjection struct {
	JobID            string
	Status           string
	RouteSnapshot    k12.GradingModelSnapshot
	ReplaceResult    bool
	AnnotatedAssetID string
	ResultMarkdown   string
	Unresolved       []string
}

func (c *PracticeReturnRegradeCoordinator) initLocked() {
	if c.active == nil {
		c.active = make(map[string]bool)
	}
	if c.runCtx == nil {
		base := c.BaseContext
		if base == nil {
			base = context.Background()
		}
		c.runCtx, c.runCancel = context.WithCancel(base)
	}
	if c.workerIdle == nil {
		c.workerIdle = make(chan struct{})
		close(c.workerIdle)
	}
}

func practiceReturnWorkerKey(agentName, setID, returnID string) string {
	return agentName + "\x00" + setID + "\x00" + returnID
}

func (c *PracticeReturnRegradeCoordinator) validate() error {
	if c == nil || c.Deps == nil || c.Deps.Records == nil || c.Grading == nil {
		return fmt.Errorf("%w: automatic regrade dependencies are incomplete", ErrInvalidInput)
	}
	return nil
}

// StartAsync schedules an already-persisted return. It never creates evidence
// and therefore cannot lose the command if the process exits before this call.
func (c *PracticeReturnRegradeCoordinator) StartAsync(agentName, setID, returnID string) bool {
	if c == nil || c.validate() != nil {
		return false
	}
	key := practiceReturnWorkerKey(agentName, setID, returnID)
	c.mu.Lock()
	c.initLocked()
	if c.sealed || c.active[key] {
		c.mu.Unlock()
		return false
	}
	c.active[key] = true
	if c.workerCount == 0 {
		c.workerIdle = make(chan struct{})
	}
	c.workerCount++
	runCtx := c.runCtx
	c.mu.Unlock()

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("K12 练习回传自动复批 worker panic",
					"agent", agentName, "set", setID, "return", returnID, "panic", recovered)
			}
			c.mu.Lock()
			delete(c.active, key)
			c.workerCount--
			if c.workerCount == 0 {
				close(c.workerIdle)
			}
			c.mu.Unlock()
		}()
		if err := c.Process(runCtx, agentName, setID, returnID); err != nil &&
			!errors.Is(err, context.Canceled) {
			slog.Warn("K12 练习回传自动复批未完成",
				"agent", agentName, "set", setID, "return", returnID, "err", err)
		}
	}()
	return true
}

// Process advances one durable return. Re-entry is idempotent: the same
// return_id binds the same GradingJob route and system conclusions are replayed
// without repeating review projection.
func (c *PracticeReturnRegradeCoordinator) Process(
	ctx context.Context,
	agentName, setID, returnID string,
) error {
	if err := c.validate(); err != nil {
		return err
	}
	view, ret, err := c.loadReturn(ctx, agentName, setID, returnID)
	if err != nil {
		return err
	}
	if ret.RegradeStatus == k12.PracticeRegradeCompleted {
		return nil
	}
	path, err := assetstore.PathFromID(ret.AssetID)
	if err != nil {
		return c.projectFailure(ctx, agentName, setID, returnID, ret,
			k12.PracticeRegradeFailedTerminal, err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return c.projectFailure(ctx, agentName, setID, returnID, ret,
			k12.PracticeRegradeFailedTerminal, err)
	}
	job, _, err := c.Grading.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: PhotoGradeRequest{
			AgentName:     agentName,
			SourceSession: view.Record.SourceSession,
			Image:         image,
			TaskIntent:    PhotoTaskCompletedHomework,
		},
		SourceKind: PracticeReturnGradingSourceKind,
		SourceKey:  setID + ":" + returnID,
	})
	if err != nil {
		return c.projectFailure(ctx, agentName, setID, returnID, ret,
			k12.PracticeRegradeFailedTerminal, err)
	}
	projection := practiceReturnRegradeProjection{
		JobID: job.Record.RecordID, Status: k12.PracticeRegradeRunning,
		RouteSnapshot: job.Fields.ModelSnapshot,
	}
	if err := c.updateProjection(ctx, agentName, setID, returnID, projection); err != nil {
		return err
	}

	for {
		job, runErr := c.Grading.RunGradingJob(ctx, job.Record.RecordID)
		switch job.Record.Status {
		case k12.GradingStageCompleted:
			return c.projectCompleted(ctx, agentName, setID, returnID, ret.ItemIDs, job)
		case k12.GradingStageAwaitingConfirmation:
			if job.Fields.ConfirmationState == k12.GradingConfirmationPending {
				questions, _ := c.Grading.RecognizedQuestionsForOwner(
					ctx, agentName, job.Record.RecordID,
				)
				unresolved := unresolvedPracticeReturnItems(ret.ItemIDs, questions)
				return c.updateProjection(ctx, agentName, setID, returnID,
					practiceReturnRegradeProjection{
						JobID: job.Record.RecordID, Status: k12.PracticeRegradeNeedsReview,
						RouteSnapshot: job.Fields.ModelSnapshot, ReplaceResult: true,
						Unresolved: unresolved,
					})
			}
			// Clear recognition has already auto-frozen but the independent
			// anchor branch may still be running. Keep the same worker/job and
			// wait for its explicit terminal state instead of mislabelling it.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
				continue
			}
		case k12.GradingStageOutcomeUnknown:
			return c.projectFailure(ctx, agentName, setID, returnID, ret,
				k12.PracticeRegradeOutcomeUnknown, runErr)
		case k12.GradingStageFailedRetryable:
			return c.projectFailure(ctx, agentName, setID, returnID, ret,
				k12.PracticeRegradeFailedRetryable, runErr)
		case k12.GradingStageFailedTerminal, k12.GradingStageCancelled:
			return c.projectFailure(ctx, agentName, setID, returnID, ret,
				k12.PracticeRegradeFailedTerminal, runErr)
		default:
			if runErr != nil {
				return c.projectFailure(ctx, agentName, setID, returnID, ret,
					k12.PracticeRegradeFailedRetryable, runErr)
			}
		}
		if runErr != nil {
			return runErr
		}
	}
}

func (c *PracticeReturnRegradeCoordinator) projectCompleted(
	ctx context.Context,
	agentName, setID, returnID string,
	itemIDs []string,
	job GradingJobView,
) error {
	result, ok := c.Grading.PhotoResult(job.Record.RecordID)
	if !ok {
		return c.updateProjection(ctx, agentName, setID, returnID,
			practiceReturnRegradeProjection{
				JobID: job.Record.RecordID, Status: k12.PracticeRegradeOutcomeUnknown,
				RouteSnapshot: job.Fields.ModelSnapshot,
			})
	}
	grades, unresolved := alignedPracticeReturnResults(itemIDs, result.Items)
	if len(grades) > 0 {
		if _, err := c.Deps.GradePracticeSetItems(ctx, agentName, setID, grades); err != nil {
			return err
		}
	}
	annotatedAssetID := ""
	if result.AnnotatedImage != nil && len(result.AnnotatedImage.Data) > 0 {
		saved, err := assetstore.Save(agentName, result.AnnotatedImage.Data)
		if err != nil {
			return err
		}
		annotatedAssetID = saved
	}
	status := k12.PracticeRegradeCompleted
	if len(unresolved) > 0 {
		status = k12.PracticeRegradeNeedsReview
	}
	return c.updateProjection(ctx, agentName, setID, returnID,
		practiceReturnRegradeProjection{
			JobID: job.Record.RecordID, Status: status,
			RouteSnapshot:    job.Fields.ModelSnapshot,
			ReplaceResult:    true,
			AnnotatedAssetID: annotatedAssetID,
			ResultMarkdown:   result.Markdown,
			Unresolved:       unresolved,
		})
}

func alignedPracticeReturnResults(
	itemIDs []string,
	items []PhotoGradeItem,
) ([]PracticeGradeResult, []string) {
	if len(itemIDs) != len(items) {
		return nil, append([]string(nil), itemIDs...)
	}
	grades := make([]PracticeGradeResult, 0, len(itemIDs))
	unresolved := make([]string, 0)
	for index, item := range items {
		switch item.Status {
		case PhotoCorrect:
			grades = append(grades, PracticeGradeResult{ItemID: itemIDs[index], Correct: true})
		case PhotoWrong:
			grades = append(grades, PracticeGradeResult{ItemID: itemIDs[index], Correct: false})
		default:
			unresolved = append(unresolved, itemIDs[index])
		}
	}
	return grades, unresolved
}

func unresolvedPracticeReturnItems(
	itemIDs []string,
	questions []RecognizedQuestion,
) []string {
	if len(itemIDs) != len(questions) {
		return append([]string(nil), itemIDs...)
	}
	unresolved := make([]string, 0)
	for index, question := range questions {
		if NormalizeRecognizedQuestion(question).ConfirmationRequired {
			unresolved = append(unresolved, itemIDs[index])
		}
	}
	if len(unresolved) == 0 {
		// Awaiting confirmation is itself evidence of an unresolved condition;
		// fail closed if a legacy recognizer omitted typed reasons.
		return append([]string(nil), itemIDs...)
	}
	return unresolved
}

func (c *PracticeReturnRegradeCoordinator) loadReturn(
	ctx context.Context,
	agentName, setID, returnID string,
) (PracticeSetView, k12.PracticeReturnAsset, error) {
	view, err := c.Deps.GetPracticeSet(ctx, strings.TrimSpace(agentName), strings.TrimSpace(setID))
	if err != nil {
		return PracticeSetView{}, k12.PracticeReturnAsset{}, err
	}
	for _, ret := range view.Fields.ReturnAssets {
		if ret.ReturnID == strings.TrimSpace(returnID) {
			return view, ret, nil
		}
	}
	return PracticeSetView{}, k12.PracticeReturnAsset{},
		fmt.Errorf("%w: practice return %q not found", records.ErrNotFound, returnID)
}

func (c *PracticeReturnRegradeCoordinator) updateProjection(
	ctx context.Context,
	agentName, setID, returnID string,
	projection practiceReturnRegradeProjection,
) error {
	for attempt := 0; attempt < 5; attempt++ {
		view, _, err := c.loadReturn(ctx, agentName, setID, returnID)
		if err != nil {
			return err
		}
		for index := range view.Fields.ReturnAssets {
			ret := &view.Fields.ReturnAssets[index]
			if ret.ReturnID != returnID {
				continue
			}
			if ret.RegradeJobID != "" && projection.JobID != "" &&
				ret.RegradeJobID != projection.JobID {
				return fmt.Errorf("%w: return_id %q already binds grading job %q",
					ErrInvalidInput, returnID, ret.RegradeJobID)
			}
			if projection.JobID != "" {
				ret.RegradeJobID = projection.JobID
			}
			if projection.Status != "" {
				ret.RegradeStatus = projection.Status
			}
			if projection.RouteSnapshot.Provider != "" || projection.RouteSnapshot.Model != "" {
				ret.RouteSnapshot = k12.NormalizeGradingModelSnapshot(projection.RouteSnapshot)
			}
			if projection.ReplaceResult {
				ret.AnnotatedAssetID = projection.AnnotatedAssetID
				ret.ResultMarkdown = projection.ResultMarkdown
				ret.UnresolvedItemIDs = append([]string(nil), projection.Unresolved...)
			}
			ret.RegradeUpdatedAt = c.Deps.now()
			break
		}
		if err := c.Deps.savePracticeFields(ctx, view, view.Record.Status); err == nil {
			return nil
		} else if !errors.Is(err, records.ErrVersionConflict) {
			return err
		}
	}
	return fmt.Errorf("%w: automatic regrade projection CAS exhausted", records.ErrVersionConflict)
}

func (c *PracticeReturnRegradeCoordinator) projectFailure(
	ctx context.Context,
	agentName, setID, returnID string,
	ret k12.PracticeReturnAsset,
	status string,
	cause error,
) error {
	if err := c.updateProjection(ctx, agentName, setID, returnID,
		practiceReturnRegradeProjection{
			JobID: ret.RegradeJobID, Status: status, RouteSnapshot: ret.RouteSnapshot,
		}); err != nil {
		return err
	}
	if cause == nil {
		return nil
	}
	return cause
}

// Recover schedules every nonterminal return projection for the supplied
// owner set. It never retries failed/outcome-unknown work blindly.
func (c *PracticeReturnRegradeCoordinator) Recover(
	ctx context.Context,
	agentNames []string,
) (int, error) {
	if err := c.validate(); err != nil {
		return 0, err
	}
	count := 0
	for _, agentName := range agentNames {
		sets, err := c.Deps.ListPracticeSets(ctx, agentName, "")
		if err != nil {
			return count, err
		}
		for _, set := range sets {
			for _, ret := range set.Fields.ReturnAssets {
				switch ret.RegradeStatus {
				case "", k12.PracticeRegradeQueued, k12.PracticeRegradeRunning:
					if c.StartAsync(agentName, set.Record.RecordID, ret.ReturnID) {
						count++
					}
				}
			}
		}
	}
	return count, nil
}

func (c *PracticeReturnRegradeCoordinator) Wait(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.initLocked()
	done := c.workerIdle
	c.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *PracticeReturnRegradeCoordinator) Shutdown(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.initLocked()
	if !c.sealed {
		c.sealed = true
		if c.runCancel != nil {
			c.runCancel()
		}
	}
	done := c.workerIdle
	c.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
