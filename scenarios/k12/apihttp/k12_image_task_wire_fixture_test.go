package apihttp_test

import (
	"os"
	"path/filepath"
	"testing"
)

const k12ImageTaskWireFixtureDirEnv = "HEXCLAW_K12_WIRE_FIXTURE_DIR"

func writeK12ImageTaskWireFixture(t *testing.T, name string, body []byte) {
	t.Helper()
	directory := os.Getenv(k12ImageTaskWireFixtureDirEnv)
	if directory == "" {
		return
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		t.Fatalf("K12 wire fixture directory is unavailable: %v", err)
	}
	path := filepath.Join(directory, filepath.Base(name))
	if err := os.WriteFile(path, append([]byte(nil), body...), 0o600); err != nil {
		t.Fatalf("write K12 wire fixture %q: %v", name, err)
	}
}
