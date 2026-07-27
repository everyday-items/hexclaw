package apihttp_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func a01BundleBody(t *testing.T, key string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(weeklyBundleBody(key, 0, 0, 0)), &body); err != nil {
		t.Fatal(err)
	}
	body["agent_config"] = map[string]any{
		"display_name":  "明明的辅导助手",
		"description":   "数学与语文家庭辅导",
		"system_prompt": "按当前教材进度辅导",
		"provider":      "",
		"model":         "",
		"skills":        []string{"chinese-tutor"},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestBUG20260726034A01ProfileBundlePersistsAgentConfigAndReplaysAtomically(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	body := a01BundleBody(t, "a01-success")
	rec, out := do(t, h, http.MethodPut, "/profile-bundle", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("profile-bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, out, "agent_config", "profile", "curriculum_progress",
		"weekly_practice_settings", "replayed")
	config := out["agent_config"].(map[string]any)
	exactKeys(t, config, "display_name", "description", "system_prompt", "provider", "model", "skills")
	if config["display_name"] != "明明的辅导助手" ||
		config["description"] != "数学与语文家庭辅导" ||
		config["system_prompt"] != "按当前教材进度辅导" {
		t.Fatalf("agent_config response drifted: %v", config)
	}
	wantSkills := []any{"chinese-tutor", "k12-pedagogy", "homework-checker", "math-tutor",
		"grade-constraint", "k12_grade", "k12_review"}
	if !reflect.DeepEqual(config["skills"], wantSkills) {
		t.Fatalf("agent_config skills not normalized: got=%v want=%v", config["skills"], wantSkills)
	}

	var displayName, description, systemPrompt, provider, model, skillsJSON string
	if err := deps.Records.DB().QueryRow(`SELECT display_name,description,
		system_prompt,provider,model,skills FROM agents WHERE name='mingming'`).Scan(
		&displayName, &description, &systemPrompt, &provider, &model, &skillsJSON); err != nil {
		t.Fatal(err)
	}
	if displayName != "明明的辅导助手" ||
		description != "数学与语文家庭辅导" ||
		systemPrompt != "按当前教材进度辅导" ||
		provider != "" || model != "" {
		t.Fatalf("agents five fields not committed with bundle: %q %q %q %q %q",
			displayName, description, systemPrompt, provider, model)
	}
	var persistedSkills []string
	if err := json.Unmarshal([]byte(skillsJSON), &persistedSkills); err != nil {
		t.Fatal(err)
	}
	wantPersistedSkills := []string{"chinese-tutor", "k12-pedagogy", "homework-checker",
		"math-tutor", "grade-constraint", "k12_grade", "k12_review"}
	if !reflect.DeepEqual(persistedSkills, wantPersistedSkills) {
		t.Fatalf("agents skills not committed with bundle: got=%v want=%v",
			persistedSkills, wantPersistedSkills)
	}

	rec, replay := do(t, h, http.MethodPut, "/profile-bundle", body)
	if rec.Code != http.StatusOK || replay["replayed"] != true {
		t.Fatalf("bundle replay status=%d body=%v", rec.Code, replay)
	}
	first := make(map[string]any, len(out))
	for key, value := range out {
		first[key] = value
	}
	first["replayed"] = true
	if !reflect.DeepEqual(replay, first) {
		t.Fatalf("same-key replay drifted: first=%v replay=%v", first, replay)
	}
}

func TestBUG20260726034A01ProfileBundleFailureRollsBackAgentAndK12State(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	db := deps.Records.DB()
	if _, err := db.Exec(`UPDATE agents SET display_name='旧名称',
		description='旧描述',system_prompt='旧人设',provider='',model='',skills='["old-skill"]'
		WHERE name='mingming'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER bug_20260726_034_a01_fail_settings
		BEFORE INSERT ON k12_weekly_practice_settings
		BEGIN SELECT RAISE(ABORT,'forced settings failure'); END`); err != nil {
		t.Fatal(err)
	}

	rec, _ := do(t, h, http.MethodPut, "/profile-bundle",
		a01BundleBody(t, "a01-forced-failure"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("forced transaction failure status=%d want 500; body=%s",
			rec.Code, rec.Body.String())
	}

	var displayName, description, systemPrompt, skillsJSON string
	if err := db.QueryRow(`SELECT display_name,description,system_prompt,skills
		FROM agents WHERE name='mingming'`).Scan(
		&displayName, &description, &systemPrompt, &skillsJSON); err != nil {
		t.Fatal(err)
	}
	if displayName != "旧名称" || description != "旧描述" ||
		systemPrompt != "旧人设" || skillsJSON != `["old-skill"]` {
		t.Errorf("failed bundle partially committed agent fields: %q %q %q %q",
			displayName, description, systemPrompt, skillsJSON)
	}
	for _, table := range []string{
		"k12_profile_revisions",
		"k12_curriculum_progress",
		"k12_weekly_practice_settings",
		"k12_profile_bundle_commands",
	} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table +
			" WHERE agent_name='mingming'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("failed bundle wrote %s rows=%d", table, count)
		}
	}
}
