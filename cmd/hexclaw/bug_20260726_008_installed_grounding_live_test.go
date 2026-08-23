package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

const (
	bug20260726008InstalledGroundingGate = "HEXCLAW_BUG_20260726_008_INSTALLED_GROUNDING_LIVE"
	bug20260726008SidecarExecutableEnv   = "HEXCLAW_CURRENT_SIDECAR_EXECUTABLE"
	bug20260726008Agent                  = "mingming"
	bug20260726008Owner                  = "desktop-user"
	bug20260726008DispatchID             = "dispatch-bug-20260726-008-live"
	bug20260726008LegacyMarker           = "BUG008_LEGACY_GROUNDING_MUST_NOT_BE_USED"
	bug20260726008TypedMarker            = "BUG008_TYPED_TEXTBOOK_SCOPE"
	bug20260726008ProviderMarker         = "BUG008_FAKE_PROVIDER_GENERAL_GUIDANCE"
)

// TestBUG20260726008InstalledGroundingUsesTypedBindingWithoutLegacyFallback
// 只在显式开启时启动当前源码构建出的 Sidecar。测试把 HOME、SQLite、配置和资产目录
// 全部隔离，并用回环 fake Provider 证明生产 composition 在 typed binding 无法冻结
// revision 时 fail-closed，而不是读取仍可由兼容路由写入的 legacy grounding。
func TestBUG20260726008InstalledGroundingUsesTypedBindingWithoutLegacyFallback(t *testing.T) {
	if strings.TrimSpace(os.Getenv(bug20260726008InstalledGroundingGate)) != "1" {
		t.Skip("set " + bug20260726008InstalledGroundingGate + "=1 to run the process-level grounding boundary")
	}
	executable := strings.TrimSpace(os.Getenv(bug20260726008SidecarExecutableEnv))
	if !filepath.IsAbs(executable) {
		t.Fatalf("%s must name an absolute current-source sidecar executable", bug20260726008SidecarExecutableEnv)
	}
	if info, err := os.Stat(executable); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("%s must name an executable file", bug20260726008SidecarExecutableEnv)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	runRoot := t.TempDir()
	home := filepath.Join(runRoot, "home")
	hexclawDir := filepath.Join(home, ".hexclaw")
	tmpDir := filepath.Join(home, "tmp")
	assetRoot := filepath.Join(home, "assets")
	for _, dir := range []string{home, hexclawDir, tmpDir, assetRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create isolated runtime directory: %v", err)
		}
	}
	dbPath := filepath.Join(hexclawDir, "data.db")
	bug20260726008SeedInstalledGroundingState(t, dbPath)

	var completionCalls atomic.Int64
	var legacyPromptLeaks atomic.Int64
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		completionCalls.Add(1)
		if bytes.Contains(body, []byte(bug20260726008LegacyMarker)) {
			legacyPromptLeaks.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"bug008-fake","object":"chat.completion","created":1,"model":"bug008-fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"`+bug20260726008ProviderMarker+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer fakeProvider.Close()

	port := bug20260726008FreeLoopbackPort(t)
	configPath := filepath.Join(hexclawDir, "hexclaw.yaml")
	bug20260726008WriteInstalledGroundingConfig(t, configPath, dbPath, fakeProvider.URL, port)
	process := bug20260726008StartSidecar(t, ctx, executable, configPath, runRoot, home, tmpDir, assetRoot, port)
	defer process.stop(t)

	client := &http.Client{Timeout: 10 * time.Second}
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	bug20260726008WaitForHealth(t, ctx, client, baseURL, process)

	optionsBefore := bug20260726008JSONRequest(t, ctx, client, http.MethodGet,
		baseURL+"/api/k12/textbook-binding-options?agent="+bug20260726008Agent+"&subject=math", nil)
	bug20260726008AssertTypedBindingOption(t, optionsBefore)

	legacyWrite := bug20260726008JSONRequest(t, ctx, client, http.MethodPost,
		baseURL+"/api/k12/grounding", map[string]any{
			"agent": bug20260726008Agent, "subject": "数学",
			"title": "legacy-only-poison", "content": bug20260726008LegacyMarker,
		})
	if ok, _ := legacyWrite["ok"].(bool); !ok {
		t.Fatalf("legacy grounding compatibility route did not acknowledge the write: keys=%v",
			bug20260726008SortedKeys(legacyWrite))
	}

	optionsAfter := bug20260726008JSONRequest(t, ctx, client, http.MethodGet,
		baseURL+"/api/k12/textbook-binding-options?agent="+bug20260726008Agent+"&subject=math", nil)
	bug20260726008AssertTypedBindingOption(t, optionsAfter)

	completionCallsBeforeTips := completionCalls.Load()
	tips := bug20260726008JSONRequest(t, ctx, client, http.MethodPost,
		baseURL+"/api/k12/tutoring-tips", map[string]any{
			"agent": bug20260726008Agent, "dispatch_id": bug20260726008DispatchID,
		})
	sections, ok := tips["sections"].([]any)
	if !ok || len(sections) != 3 {
		t.Fatalf("tutoring sections=%T/%d, want exact three-section projection", tips["sections"], len(sections))
	}
	encoded, err := json.Marshal(tips)
	if err != nil {
		t.Fatal(err)
	}
	projection := string(encoded)
	if !strings.Contains(projection, bug20260726008ProviderMarker) {
		t.Fatalf("tutoring projection did not use the local fake Provider")
	}
	if strings.Contains(projection, bug20260726008LegacyMarker) {
		t.Fatalf("production composition fell back to legacy grounding")
	}
	if strings.Contains(projection, bug20260726008TypedMarker) {
		t.Fatalf("unfrozen typed textbook content escaped without an active revision")
	}
	firstSection, _ := sections[0].(map[string]any)
	if firstSection["source_label"] != usecase.TutoringTipsSourceAI {
		t.Fatalf("unfrozen typed scope source_label=%v, want %q", firstSection["source_label"], usecase.TutoringTipsSourceAI)
	}
	if delta := completionCalls.Load() - completionCallsBeforeTips; delta != 1 {
		t.Fatalf("fake Provider tutoring completion delta=%d, want one durable concept fallback", delta)
	}
	if legacyPromptLeaks.Load() != 0 {
		t.Fatalf("legacy grounding marker reached the fake Provider prompt")
	}
}

func bug20260726008SeedInstalledGroundingState(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open isolated SQLite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrate.Run(t.Context(), db, migrate.All); err != nil {
		t.Fatalf("migrate isolated SQLite: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, bug20260726008Agent); err != nil {
		t.Fatalf("seed isolated agent: %v", err)
	}
	metadata, err := json.Marshal(k12.ApplyProfileToMeta(nil, k12.ChildProfile{
		ChildName: "小明", GradeTerm: "五年级下", TextbookEdition: "人教版",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agents SET metadata=? WHERE name=?`, string(metadata), bug20260726008Agent); err != nil {
		t.Fatalf("seed isolated profile metadata: %v", err)
	}

	registry := scenario.NewRegistry()
	constraint := curriculum.New()
	if err := registry.Assemble(k12.Pack(constraint)); err != nil {
		t.Fatalf("assemble isolated K12 record schemas: %v", err)
	}
	records := k12storage.NewStore(db, registry.Records)
	deps := usecase.Deps{
		Records: records, Constraint: constraint, TextbookOwnerID: bug20260726008Owner,
		Now: func() int64 { return 1_000 },
	}
	snapshot := k12.ProblemAttemptSnapshot{
		Problems: []k12.Problem{{
			ProblemID: "problem-bug008", AgentName: bug20260726008Agent,
			SubmissionID: "submission-bug008", PageAssetID: "asset-bug008", Ordinal: 0,
			ProblemKind: k12.ProblemKindStandalone, Subject: "数学",
			StemRaw: "四分之一加四分之二是多少？",
			StemMarkdown: "$\\frac{1}{4}+\\frac{2}{4}=?$",
			ConceptIDs: []string{"同分母分数加法"}, CanonicalVersion: 1,
			CreatedAt: 1_000, UpdatedAt: 1_000,
		}},
		Attempts: []k12.Attempt{{
			AttemptID: "attempt-bug008", AgentName: bug20260726008Agent,
			SubmissionID: "submission-bug008", ProblemID: "problem-bug008",
			AnswerState: "present", AnswerRaw: "3/4", AnswerMarkdown: "$\\frac{3}{4}$",
			ConfirmedVersion: 1, InputDigest: "pending", CreatedAt: 1_000, UpdatedAt: 1_000,
		}},
	}
	questions, err := usecase.RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		t.Fatalf("derive confirmed problem facts: %v", err)
	}
	frozen := usecase.FreezeRecognizedQuestionInputDigests(questions, "五年级下")
	snapshot.Attempts[0].InputDigest = frozen[0].InputDigest
	if err := records.PutProblemAttemptSnapshot(t.Context(), snapshot); err != nil {
		t.Fatalf("seed confirmed problem facts: %v", err)
	}

	job, created, err := deps.CreateGradingJob(t.Context(), bug20260726008Agent, "session-bug008", usecase.CreateGradingJobInput{
		SubmissionID: "submission-bug008", SourceKind: "im", SourceKey: "message-bug008",
		ModelSnapshot: k12.GradingModelSnapshot{
			Provider: "bug008-fake", Model: "bug008-fake-model", Capability: "vision",
		},
	})
	if err != nil || !created {
		t.Fatalf("seed grading job: created=%t err=%v", created, err)
	}
	advance := func(outcome, digest string) {
		t.Helper()
		job, err = deps.AdvanceGradingStage(t.Context(), bug20260726008Agent, job.Record.RecordID,
			usecase.AdvanceGradingInput{Outcome: outcome, ArtifactDigest: digest})
		if err != nil {
			t.Fatalf("advance seeded grading job: %v", err)
		}
	}
	advance(usecase.GradingOutcomeOK, "")
	advance(usecase.GradingOutcomeOK, "normalize-bug008")
	advance(usecase.GradingOutcomeOK, "recognize-bug008")
	job, err = deps.AdvanceGradingStage(t.Context(), bug20260726008Agent, job.Record.RecordID,
		usecase.AdvanceGradingInput{
			Outcome: usecase.GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated,
			ArtifactDigest: "anchor-bug008",
		})
	if err != nil {
		t.Fatalf("seed grading anchor: %v", err)
	}
	job, err = deps.ConfirmGradingJob(t.Context(), bug20260726008Agent, job.Record.RecordID, []string{"confirmed"})
	if err != nil || job.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("seed confirmed grading job: status=%s err=%v", job.Record.Status, err)
	}

	bug20260726008SeedTypedTextbookBinding(t, db)
	bug20260726008SeedImageTaskBridge(t, db, job.Record.RecordID)
}

func bug20260726008SeedTypedTextbookBinding(t *testing.T, db *sql.DB) {
	t.Helper()
	digest := strings.Repeat("a", 64)
	catalogDigest := strings.Repeat("b", 64)
	catalog := `{"subject":"math","textbook_edition":"人教版","textbook_version":"2022","title":"义务教育教科书·数学五年级下册","volume":"下册","page_min":1,"page_max":1,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":1,"lessons":[]}],"page_refs":[{"logical_page":1,"pdf_page":1,"segment_refs":["segment-bug008"]}]}`
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO kb_semantic_corpora
			(corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
			VALUES('corpus-bug008',?,'default','general',1,1,1)`, []any{bug20260726008Owner}},
		{`INSERT INTO kb_documents
			(id,title,content,source,deleted,corpus_uid,created_at,updated_at)
			VALUES('doc-bug008','数学五年级下册.pdf',?,'upload:textbook.pdf',0,
			'corpus-bug008',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, []any{bug20260726008TypedMarker}},
		{`INSERT INTO kb_chunks
			(id,doc_id,content,chunk_index,created_at,page_start,page_end,source_digest,
			 source_offset_start,source_offset_end)
			VALUES('segment-bug008','doc-bug008',?,0,CURRENT_TIMESTAMP,1,1,?,0,?)`,
			[]any{bug20260726008TypedMarker, digest, len(bug20260726008TypedMarker)}},
		{`INSERT INTO kb_semantic_document_generations
			(owner_id,corpus_uid,document_id,content_generation,created_at)
			VALUES(?,'corpus-bug008','doc-bug008',1,1)`, []any{bug20260726008Owner}},
		{`INSERT INTO kb_semantic_document_bindings
			(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,
			 version,created_at,updated_at)
			VALUES('doc-bug008',?,'corpus-bug008',1,'active','ready',1,1,1)`, []any{bug20260726008Owner}},
		{`INSERT INTO k12_textbook_manifests
			(manifest_id,owner_id,document_id,document_generation,document_title,subject,
			 source_digest,state,retryable,failure_message,text_index_state,vector_index_state,
			 catalog_json,catalog_digest,created_at,updated_at)
			VALUES('manifest-bug008',?,'doc-bug008',1,'数学五年级下册.pdf','math',?,
			'ready_for_confirmation',0,'','ready','ready',?,?,1,1)`,
			[]any{bug20260726008Owner, digest, catalog, catalogDigest}},
		{`INSERT INTO k12_textbook_page_mappings
			(mapping_id,manifest_id,logical_page,pdf_page,evidence_page,evidence_offset_start,
			 evidence_offset_end,evidence_digest,method,verification_state,document_id,
			 document_generation,source_digest,created_at,updated_at)
			VALUES('mapping-bug008','manifest-bug008',1,1,1,0,1,?,'printed_anchor',
			'verified','doc-bug008',1,?,1,1)`, []any{strings.Repeat("c", 64), digest}},
		{`INSERT INTO k12_textbook_manifest_segments
			(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
			 document_generation,source_digest,created_at,updated_at)
			VALUES('manifest-segment-bug008','manifest-bug008',1,'segment-bug008',1,
			'doc-bug008',1,?,1,1)`, []any{digest}},
		{`INSERT INTO k12_textbook_bindings
			(textbook_binding_id,owner_id,agent_name,subject,textbook_manifest_id,
			 document_id,document_generation,status,created_at,updated_at)
			VALUES('binding-bug008',?,?,'math','manifest-bug008','doc-bug008',1,'active',1,1)`,
			[]any{bug20260726008Owner, bug20260726008Agent}},
	}
	for index, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed typed textbook statement %d: %v", index, err)
		}
	}
}

func bug20260726008SeedImageTaskBridge(t *testing.T, db *sql.DB, gradingJobID string) {
	t.Helper()
	const submissionID = "submission-bug008"
	_, err := db.Exec(`INSERT INTO k12_image_task_dispatches
		(dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
		 source_asset_refs_json,source_digest,message_intent,task_intent,intent_evidence_json,
		 intent_confidence,confirmation_candidates_json,status,target_object_type,target_object_id,
		 classification_route_snapshot_json,classification_invocation_id,route_policy_snapshot_json,
		 idempotency_key,request_digest,attempt_generation,retry_safe,failure_kind,version,
		 created_at,updated_at)
		VALUES(?,?,?,'desktop','bug008-live','session-bug008','[]',?,'','completed_homework',
		'[]',1,'[]','routed','homework_submission',?,?,'classification-bug008','{}',
		'idempotency-bug008',?,1,0,'',1,1,1)`,
		bug20260726008DispatchID, bug20260726008Agent, bug20260726008Agent,
		strings.Repeat("d", 64), submissionID,
		`{"provider":"bug008-fake","model":"bug008-fake-model","route":"bug008-fake/bug008-fake-model","capability":"vision","selection_source":"explicit","policy_version":"bug008-live","prompt_version":"bug008-live"}`,
		strings.Repeat("e", 64),
	)
	if err != nil {
		t.Fatalf("seed image task dispatch: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO k12_homework_submissions
		(submission_id,dispatch_id,agent_name,learner_id,source_kind,source_ref,
		 source_asset_refs_json,task_intent,status,grading_job_id,idempotency_key,version,
		 created_at,updated_at)
		VALUES(?,?,?,?,'desktop','bug008-live','[]','completed_homework',
		'awaiting_confirmation',?,'homework-bug008',1,1,1)`,
		submissionID, bug20260726008DispatchID, bug20260726008Agent,
		bug20260726008Agent, gradingJobID,
	); err != nil {
		t.Fatalf("seed homework dispatch bridge: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO k12_image_task_owner_scopes
		(dispatch_id,owner_scope,agent_name,created_at) VALUES(?,?,?,1)`,
		bug20260726008DispatchID, bug20260726008Owner, bug20260726008Agent,
	); err != nil {
		t.Fatalf("seed image task owner scope: %v", err)
	}
}

func bug20260726008WriteInstalledGroundingConfig(
	t *testing.T,
	configPath, dbPath, providerURL string,
	port int,
) {
	t.Helper()
	enabled := true
	cfg := config.DefaultConfig()
	cfg.Server = config.ServerConfig{Host: "127.0.0.1", Port: port, MCPPort: port + 1, Mode: "production"}
	cfg.Storage = config.StorageConfig{Driver: "sqlite", SQLite: config.SQLiteConfig{Path: dbPath}}
	cfg.LLM = config.LLMConfig{
		Default: "bug008-fake",
		Providers: map[string]config.LLMProviderConfig{
			"bug008-fake": {
				APIKey: "test-only-not-a-secret", BaseURL: providerURL + "/v1",
				Model: "bug008-fake-model",
				Models: []string{"bug008-fake-model"}, Compatible: "openai",
				Locality: config.ProviderLocalityLocal, Enabled: &enabled,
			},
		},
		Routing: config.LLMRoutingConfig{Enabled: false},
		Cache: config.LLMCacheConfig{Enabled: false},
		Tools: config.LLMToolsConfig{Enabled: "off"},
	}
	cfg.Router.Enabled = true
	cfg.Router.DefaultAgent = bug20260726008Agent
	cfg.Router.LLMFallback = false
	cfg.Knowledge.Enabled = true
	cfg.Knowledge.QueryExpand = false
	cfg.Knowledge.Contextual = false
	cfg.Knowledge.Rerank = false
	cfg.Knowledge.Embedding = config.EmbeddingConfig{DisableAutoInstall: true}
	cfg.Platforms = config.PlatformsConfig{}
	cfg.MCP = config.MCPConfig{}
	cfg.Skills = config.SkillsConfig{}
	cfg.Heartbeat = config.HeartbeatConfig{}
	cfg.Cron = config.CronConfig{}
	cfg.Webhook = config.WebhookConfig{}
	cfg.FileMemory = config.FileMemoryConfig{}
	if err := config.Save(cfg, configPath); err != nil {
		t.Fatalf("save isolated sidecar configuration: %v", err)
	}
}

type bug20260726008SidecarProcess struct {
	cmd      *exec.Cmd
	done     chan error
	logFile  *os.File
	redact   []string
	stopOnce sync.Once
}

func bug20260726008StartSidecar(
	t *testing.T,
	ctx context.Context,
	executable, configPath, runRoot, home, tmpDir, assetRoot string,
	port int,
) *bug20260726008SidecarProcess {
	t.Helper()
	logFile, err := os.OpenFile(filepath.Join(runRoot, "sidecar.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create isolated sidecar log: %v", err)
	}
	cmd := exec.CommandContext(ctx, executable, "serve", "--desktop", "--config", configPath)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"USERPROFILE="+home,
		"CFFIXED_USER_HOME="+home,
		"TMPDIR="+tmpDir,
		"TEMP="+tmpDir,
		"TMP="+tmpDir,
		"HEXCLAW_ASSET_ROOT="+assetRoot,
		"HEXCLAW_SIDECAR_PORT="+strconv.Itoa(port),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start current-source sidecar: %v", err)
	}
	process := &bug20260726008SidecarProcess{
		cmd: cmd, done: make(chan error, 1), logFile: logFile,
		redact: []string{runRoot, home, tmpDir, assetRoot, configPath},
	}
	go func() { process.done <- cmd.Wait() }()
	return process
}

func (p *bug20260726008SidecarProcess) stop(t *testing.T) {
	t.Helper()
	p.stopOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			select {
			case <-p.done:
			case <-time.After(2 * time.Second):
				t.Errorf("sidecar did not stop after kill")
			}
		}
		_ = p.logFile.Close()
	})
}

func bug20260726008WaitForHealth(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	baseURL string,
	process *bug20260726008SidecarProcess,
) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-process.done:
			process.done <- err
			t.Fatalf("current-source sidecar exited before health: %v; log_tail=%q",
				err, process.sanitizedLogTail())
		case <-deadline.C:
			t.Fatal("current-source sidecar did not become healthy within 30 seconds")
		case <-ctx.Done():
			t.Fatalf("sidecar startup context ended: %v", ctx.Err())
		case <-ticker.C:
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return
				}
			}
		}
	}
}

func (p *bug20260726008SidecarProcess) sanitizedLogTail() string {
	data, err := os.ReadFile(p.logFile.Name())
	if err != nil {
		return "unavailable"
	}
	if len(data) > 8<<10 {
		data = data[len(data)-(8<<10):]
	}
	result := string(data)
	for _, value := range p.redact {
		if value != "" {
			result = strings.ReplaceAll(result, value, "<isolated>")
		}
	}
	return result
}

func bug20260726008JSONRequest(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	method, url string,
	payload any,
) map[string]any {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s request failed: %v", method, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		t.Fatalf("%s response status=%d was not JSON", method, response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("%s response status=%d keys=%v", method, response.StatusCode, bug20260726008SortedKeys(decoded))
	}
	return decoded
}

func bug20260726008AssertTypedBindingOption(t *testing.T, response map[string]any) {
	t.Helper()
	items, ok := response["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("typed binding options=%T/%d, want one durable manifest", response["items"], len(items))
	}
	item, _ := items[0].(map[string]any)
	if item["manifest_id"] != "manifest-bug008" || item["document_id"] != "doc-bug008" ||
		item["state"] != "ready_for_confirmation" {
		t.Fatalf("typed binding option identity/state drifted: keys=%v", bug20260726008SortedKeys(item))
	}
}

func bug20260726008SortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func bug20260726008FreeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate loopback port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
