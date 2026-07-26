package apihttp_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func mutateBundleProfile(body string, mutate func(map[string]any)) string {
	var request map[string]any
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		panic(err)
	}
	mutate(request["profile"].(map[string]any))
	encoded, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestProfileBundleRejectsNonCanonicalSubjectTextbooksWithoutWrites(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "legacy request scalar",
			mutate: func(body string) string {
				return mutateBundleProfile(body, func(profile map[string]any) {
					delete(profile, "subject_textbooks")
					profile["textbook_edition"] = "人教版"
				})
			},
		},
		{
			name: "missing subject",
			mutate: func(body string) string {
				return mutateBundleProfile(body, func(profile map[string]any) {
					delete(profile["subject_textbooks"].(map[string]any), "art")
				})
			},
		},
		{
			name: "unknown subject",
			mutate: func(body string) string {
				return mutateBundleProfile(body, func(profile map[string]any) {
					profile["subject_textbooks"].(map[string]any)["history"] = "统编版"
				})
			},
		},
		{
			name: "empty after trim",
			mutate: func(body string) string {
				return mutateBundleProfile(body, func(profile map[string]any) {
					profile["subject_textbooks"].(map[string]any)["math"] = "   "
				})
			},
		},
		{
			name: "non string",
			mutate: func(body string) string {
				return mutateBundleProfile(body, func(profile map[string]any) {
					profile["subject_textbooks"].(map[string]any)["math"] = 7
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, deps, _ := newWeeklyContractServer(t)
			body := tt.mutate(weeklyBundleBody("invalid-bundle", 0, 0, 0))
			rec, _ := do(t, h, http.MethodPut, "/profile-bundle", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var raw string
			if err := deps.Records.DB().QueryRow(
				`SELECT metadata FROM agents WHERE name='mingming'`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var meta map[string]string
			if err := json.Unmarshal([]byte(raw), &meta); err != nil {
				t.Fatal(err)
			}
			for key := range meta {
				if strings.HasPrefix(key, "k12.") {
					t.Fatalf("invalid request wrote profile metadata: %v", meta)
				}
			}
		})
	}
}

func TestProfileBundlePersistsCanonicalSubjectsAndLegacyPUTPatchesOnlyMath(t *testing.T) {
	h, deps, _ := newWeeklyContractServer(t)
	rec, body := do(t, h, http.MethodPut, "/profile-bundle",
		weeklyBundleBody("canonical-bundle", 0, 0, 0))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	profile := body["profile"].(map[string]any)
	exactKeys(t, profile, "child_name", "grade_term", "subject_textbooks",
		"textbook_edition")
	textbooks := profile["subject_textbooks"].(map[string]any)
	exactKeys(t, textbooks, "math", "chinese", "english", "science",
		"information_technology", "art")
	if profile["textbook_edition"] != textbooks["math"] {
		t.Fatalf("response scalar is not derived math: %v", profile)
	}

	assertMetadata := func(math string) {
		t.Helper()
		var raw string
		if err := deps.Records.DB().QueryRow(
			`SELECT metadata FROM agents WHERE name='mingming'`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var meta map[string]string
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"k12.textbook_edition.math":                   math,
			"k12.textbook_edition.chinese":                "统编版",
			"k12.textbook_edition.english":                "外研版",
			"k12.textbook_edition.science":                "教科版",
			"k12.textbook_edition.information_technology": "浙教版",
			"k12.textbook_edition.art":                    "人美版",
			"k12.textbook_edition":                        math,
		}
		for key, value := range want {
			if meta[key] != value {
				t.Fatalf("metadata[%q]=%q want %q; all=%v", key, meta[key], value, meta)
			}
		}
	}
	assertMetadata("人教版")

	rec, _ = do(t, h, http.MethodPut, "/profile",
		`{"agent":"mingming","child_name":"明明","grade_term":"五年级下","textbook_edition":"北师大版"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy profile status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertMetadata("北师大版")
}
