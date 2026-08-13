//go:build testtools

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/config"
)

const (
	realK12Provider = "hexclaw-gpt"
	realK12Model    = "gpt-5.6-sol"
)

var candidatePolicyFields = []string{
	"assessing_buckets",
	"item_concurrency",
	"locating_seconds",
	"normalizing_seconds",
	"policy_version",
	"projecting_seconds",
	"queued_seconds",
	"recognition_plan_version",
	"recognizing_seconds",
	"rendering_seconds",
}

type candidatePolicyWire struct {
	PolicyVersion          int   `json:"policy_version"`
	QueuedSeconds          int64 `json:"queued_seconds"`
	NormalizingSeconds     int64 `json:"normalizing_seconds"`
	RecognizingSeconds     int64 `json:"recognizing_seconds"`
	LocatingSeconds        int64 `json:"locating_seconds"`
	RenderingSeconds       int64 `json:"rendering_seconds"`
	ProjectingSeconds      int64 `json:"projecting_seconds"`
	RecognitionPlanVersion int   `json:"recognition_plan_version"`
	AssessingBuckets       []struct {
		MaxProblems int   `json:"max_problems"`
		Seconds     int64 `json:"seconds"`
	} `json:"assessing_buckets"`
	ItemConcurrency int `json:"item_concurrency"`
}

func privateRegularFile(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", errors.New(label + " must be absolute")
	}
	link, err := os.Lstat(path)
	if err != nil || link.Mode()&os.ModeSymlink != 0 || !link.Mode().IsRegular() {
		return "", errors.New(label + " must be a regular non-symlink file")
	}
	if link.Mode().Perm() != 0o600 {
		return "", errors.New(label + " permissions must be 0600")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New(label + " cannot be resolved")
	}
	return resolved, nil
}

func parseCandidatePolicy(path string) (config.K12GradingBudgetConfig, error) {
	resolved, err := privateRegularFile(path, "candidate policy")
	if err != nil {
		return config.K12GradingBudgetConfig{}, err
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return config.K12GradingBudgetConfig{}, errors.New("cannot read candidate policy")
	}
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return config.K12GradingBudgetConfig{}, errors.New("candidate policy JSON is invalid")
	}
	if err := requireExactFieldSet(fields, candidatePolicyFields, "candidate policy"); err != nil {
		return config.K12GradingBudgetConfig{}, err
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire candidatePolicyWire
	if err := decoder.Decode(&wire); err != nil {
		return config.K12GradingBudgetConfig{}, errors.New("candidate policy is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return config.K12GradingBudgetConfig{}, errors.New("candidate policy has trailing data")
	}
	budget := config.K12GradingBudgetConfig{
		PolicyVersion:          wire.PolicyVersion,
		QueuedSeconds:          wire.QueuedSeconds,
		NormalizingSeconds:     wire.NormalizingSeconds,
		RecognizingSeconds:     wire.RecognizingSeconds,
		LocatingSeconds:        wire.LocatingSeconds,
		RenderingSeconds:       wire.RenderingSeconds,
		ProjectingSeconds:      wire.ProjectingSeconds,
		RecognitionPlanVersion: wire.RecognitionPlanVersion,
		ItemConcurrency:        wire.ItemConcurrency,
	}
	for _, bucket := range wire.AssessingBuckets {
		budget.AssessingBuckets = append(budget.AssessingBuckets, config.K12AssessingBudgetBucketConfig{
			MaxProblems: bucket.MaxProblems,
			Seconds:     bucket.Seconds,
		})
	}
	probe := config.DefaultConfig()
	probe.K12.GradingBudget = budget
	if err := probe.Validate(); err != nil || budget.IsZero() {
		return config.K12GradingBudgetConfig{}, errors.New("candidate policy is incomplete")
	}
	return budget, nil
}

func requireExactFieldSet(fields map[string]json.RawMessage, expected []string, label string) error {
	if len(fields) != len(expected) {
		return errors.New(label + " fields do not match the exact schema")
	}
	actual := make([]string, 0, len(fields))
	for field := range fields {
		actual = append(actual, field)
	}
	sort.Strings(actual)
	if !equalStrings(actual, expected) {
		return errors.New(label + " fields do not match the exact schema")
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func configuredRealModel(provider config.LLMProviderConfig) bool {
	if provider.Model == realK12Model {
		return true
	}
	for _, model := range provider.Models {
		if model == realK12Model {
			return true
		}
	}
	for _, specification := range provider.ModelSpecs {
		if specification.ID == realK12Model {
			return true
		}
	}
	return false
}

func exactRealModelSpec(provider config.LLMProviderConfig) []config.LLMProviderModelSpec {
	for _, specification := range provider.ModelSpecs {
		if specification.ID == realK12Model {
			return []config.LLMProviderModelSpec{specification}
		}
	}
	return nil
}

func executePrepareProfile(options prepareProfileOptions, stdout io.Writer) error {
	resolved, err := resolveCommon(options.commonOptions)
	if err != nil {
		return err
	}
	sourcePath, err := privateRegularFile(options.sourceConfig, "source config")
	if err != nil {
		return err
	}
	budget, err := parseCandidatePolicy(options.candidatePolicy)
	if err != nil {
		return err
	}

	return withProfileLock(resolved.profile, func() error {
		outputPath := filepath.Join(resolved.profile, ".hexclaw", "hexclaw.yaml")
		if _, err := os.Lstat(outputPath); err == nil {
			return errors.New("isolated Sidecar config already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("isolated Sidecar config cannot be inspected")
		}

		source, err := config.Load(sourcePath)
		if err != nil {
			return errors.New("source config is not usable")
		}
		provider, exists := source.LLM.Providers[realK12Provider]
		if !exists || provider.Enabled != nil && !*provider.Enabled || !configuredRealModel(provider) {
			return errors.New("source config does not contain the exact authorized provider/model")
		}
		if provider.DisplayName != "HexClaw-GPT" || strings.TrimSpace(provider.APIKey) == "" {
			return errors.New("source config does not contain an enabled exact provider identity")
		}

		provider.Model = realK12Model
		provider.Models = []string{realK12Model}
		provider.ModelSpecs = exactRealModelSpec(provider)
		if provider.ModelSpecsMode == "explicit" && len(provider.ModelSpecs) != 1 {
			return errors.New("source config does not declare the exact model capability")
		}

		prepared := config.DefaultConfig()
		prepared.Server.Host = "127.0.0.1"
		prepared.Server.Port = options.port
		prepared.LLM.Default = realK12Provider
		prepared.LLM.Providers = map[string]config.LLMProviderConfig{realK12Provider: provider}
		prepared.LLM.ReasoningProvider = realK12Provider
		prepared.LLM.ReasoningModel = realK12Model
		prepared.Platforms = config.PlatformsConfig{Web: config.WebConfig{Enabled: true}}
		prepared.Heartbeat.Enabled = false
		prepared.Cron.Enabled = false
		prepared.Webhook.Enabled = false
		prepared.Storage.Driver = "sqlite"
		prepared.Storage.SQLite.Path = resolved.store
		prepared.K12.GradingBudget = budget
		if err := prepared.Validate(); err != nil {
			return errors.New("prepared isolated config is invalid")
		}
		if err := config.Save(prepared, outputPath); err != nil {
			return errors.New("cannot write isolated Sidecar config")
		}
		info, err := os.Lstat(outputPath)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return errors.New("isolated Sidecar config was not written privately")
		}
		raw, err := os.ReadFile(outputPath)
		if err != nil {
			return errors.New("cannot attest isolated Sidecar config")
		}
		digest := sha256.Sum256(raw)
		return writeJSON(stdout, map[string]any{
			"config_sha256": fmt.Sprintf("%x", digest[:]),
			"status":        "prepared",
		})
	})
}
