package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

func TestProcessResourceGovernorMapsConfiguredLimits(t *testing.T) {
	cfg := config.DefaultConfig().ResourceGovernor
	cfg.VLMConcurrency = 2
	cfg.AcceleratorConcurrency = 3
	governor, err := newProcessResourceGovernor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	metrics := governor.Snapshot().Resources
	if metrics[resourcegov.ResourceVLM].Capacity != 2 ||
		metrics[resourcegov.ResourceAccelerator].Capacity != 3 ||
		metrics[resourcegov.ResourceCPUHeavy].Capacity != cfg.CPUHeavyConcurrency ||
		metrics[resourcegov.ResourceSQLiteWrite].Capacity != cfg.SQLiteWriteConcurrency {
		t.Fatalf("configured process limits not preserved: %+v", metrics)
	}
}

func TestProductionAssemblyCreatesOneGovernorAndInjectsEveryHeavyChain(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	newCalls := 0
	err := filepath.WalkDir(repositoryRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		newCalls += strings.Count(string(data), "resourcegov.New(")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if newCalls != 1 {
		t.Fatalf("production resourcegov.New calls=%d, want exactly 1 composition-root instance", newCalls)
	}

	mainSource, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, injection := range []string{
		"withKnowledgeSemanticResourceGovernor(processResources)",
		"knowledge.WithResourceGovernor(processResources)",
		"api.WithKnowledgeResourceGovernor(processResources)",
		"k12engineadapter.WithRecognizerResourceGovernor(processResources)",
	} {
		if !strings.Contains(string(mainSource), injection) {
			t.Errorf("production main is missing shared governor injection %q", injection)
		}
	}
	runtimeSource, err := os.ReadFile("semantic_index_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, injection := range []string{
		"knowledge.WithRevisionSearchResourceGovernor(assembly.governor)",
		"knowledge.WithSemanticWorkerResourceGovernor(assembly.governor)",
	} {
		if !strings.Contains(string(runtimeSource), injection) {
			t.Errorf("semantic runtime is missing shared governor injection %q", injection)
		}
	}
}
