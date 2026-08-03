package assetstore

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnsure_DurabilityOrderAndNoTempLeak(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HEXCLAW_ASSET_ROOT", root)
	data := durabilityPNG(t)
	var events []string
	var temp *os.File
	ops := publishDurability{
		syncFile: func(file *os.File) error {
			events = append(events, "file-sync")
			temp = file
			return file.Sync()
		},
		syncDir: func(dir string) error {
			events = append(events, "dir-sync")
			if temp == nil {
				return errors.New("directory sync happened before temporary file sync")
			}
			if _, err := temp.Stat(); err == nil {
				return errors.New("temporary file remained open when final link was published")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			if len(entries) != 1 || strings.HasPrefix(entries[0].Name(), ".upload-") {
				return errors.New("directory sync happened before temporary entry cleanup")
			}
			return syncTestDirectory(dir)
		},
	}

	id, created, err := ensureWithDurability("mingming", data, ops)
	if err != nil || !created {
		t.Fatalf("durable first publish: id=%q created=%v err=%v", id, created, err)
	}
	if !reflect.DeepEqual(events, []string{"file-sync", "dir-sync"}) {
		t.Fatalf("durability order=%v, want [file-sync dir-sync]", events)
	}
	if _, err := PathFromID(id); err != nil {
		t.Fatalf("durably published asset is missing: %v", err)
	}
}

func TestEnsure_FileSyncFailureNeverPublishes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HEXCLAW_ASSET_ROOT", root)
	syncFailure := errors.New("injected file sync failure")
	directorySyncCalled := false
	ops := publishDurability{
		syncFile: func(*os.File) error { return syncFailure },
		syncDir: func(string) error {
			directorySyncCalled = true
			return nil
		},
	}

	if _, _, err := ensureWithDurability("mingming", durabilityPNG(t), ops); !errors.Is(err, syncFailure) {
		t.Fatalf("file sync failure must propagate: %v", err)
	}
	if directorySyncCalled {
		t.Fatal("directory sync must not run after file sync failure")
	}
	entries, err := os.ReadDir(filepath.Join(root, "mingming"))
	if err != nil {
		t.Fatalf("read agent directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("file sync failure published or leaked files: %v", entries)
	}
}

func TestEnsure_DirectorySyncFailureKeepsContentAddressedFileAndRetrySyncs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HEXCLAW_ASSET_ROOT", root)
	data := durabilityPNG(t)
	inspection, err := Inspect("mingming", data)
	if err != nil {
		t.Fatal(err)
	}
	directoryFailure := errors.New("injected directory sync failure")
	firstOps := publishDurability{
		syncFile: func(file *os.File) error { return file.Sync() },
		syncDir:  func(string) error { return directoryFailure },
	}

	if _, _, err := ensureWithDurability("mingming", data, firstOps); !errors.Is(err, directoryFailure) {
		t.Fatalf("directory sync failure must propagate: %v", err)
	}
	if _, err := PathFromID(inspection.AssetID); err != nil {
		t.Fatalf("directory sync failure must not delete the shared final link: %v", err)
	}

	fileSyncCalled := false
	directorySyncCalls := 0
	retryOps := publishDurability{
		syncFile: func(*os.File) error {
			fileSyncCalled = true
			return nil
		},
		syncDir: func(dir string) error {
			directorySyncCalls++
			return syncTestDirectory(dir)
		},
	}
	id, created, err := ensureWithDurability("mingming", data, retryOps)
	if err != nil || created || id != inspection.AssetID {
		t.Fatalf("retry must durably reuse final link: id=%q created=%v err=%v", id, created, err)
	}
	if fileSyncCalled || directorySyncCalls != 1 {
		t.Fatalf("existing retry durability calls: file=%v dir=%d, want false/1", fileSyncCalled, directorySyncCalls)
	}
}

func durabilityPNG(t *testing.T) []byte {
	t.Helper()
	const encoded = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func syncTestDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
