package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestDeletedKnowledgeEmbeddingOptsOutOfAutoInstall_BUG20260713(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".hexclaw"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	s := &Server{cfg: cfg}
	s.SetKnowledgeEmbeddingInfo(KnowledgeEmbeddingInfo{
		Enabled:  true,
		Provider: "Ollama (local)",
		Model:    "nomic-embed-text",
		Local:    true,
	})

	if err := s.disableEmbeddingAutoInstallForDeletedModel("nomic-embed-text:latest"); err != nil {
		t.Fatal(err)
	}
	if !s.cfg.Knowledge.Embedding.DisableAutoInstall {
		t.Fatal("deleting the active local embedding model must disable automatic reinstall")
	}

	persisted, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Knowledge.Embedding.DisableAutoInstall {
		t.Fatal("automatic reinstall opt-out was not persisted")
	}
}

func TestDeletingOtherOllamaModelKeepsEmbeddingAutoInstall_BUG20260713(t *testing.T) {
	cfg := config.DefaultConfig()
	s := &Server{cfg: cfg}
	s.SetKnowledgeEmbeddingInfo(KnowledgeEmbeddingInfo{
		Enabled: true,
		Model:   "nomic-embed-text",
		Local:   true,
	})

	if err := s.disableEmbeddingAutoInstallForDeletedModel("qwen3.5:9b"); err != nil {
		t.Fatal(err)
	}
	if s.cfg.Knowledge.Embedding.DisableAutoInstall {
		t.Fatal("deleting an unrelated chat model must not disable embedding auto-install")
	}
}
