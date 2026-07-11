package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLogFileSink_RotateAtExactMaxSize(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  200, // very small
		MaxFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// Write entries until we exceed maxSize exactly or just past it
	for i := 0; i < 50; i++ {
		sink.Write(LogEntry{
			Timestamp: "2025-01-01T00:00:00Z",
			Level:     "info",
			Message:   fmt.Sprintf("msg-%d", i),
		})
	}

	// Current file should be small (post-rotation)
	info, err := os.Stat(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 200 {
		t.Errorf("current file should be <= maxSize after rotation, got %d", info.Size())
	}

	// At least one rotated file should exist
	if _, err := os.Stat(filepath.Join(dir, "test.log.1")); os.IsNotExist(err) {
		t.Error("test.log.1 should exist after rotation")
	}
}

func TestLogFileSink_RotateMaxFiles1(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  100,
		MaxFiles: 1, // only 1 backup
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// Write enough to trigger multiple rotations
	for i := 0; i < 100; i++ {
		sink.Write(LogEntry{
			Timestamp: "2025-01-01T00:00:00Z",
			Level:     "info",
			Message:   fmt.Sprintf("message-number-%d-padding", i),
		})
	}

	// test.log.1 should exist
	if _, err := os.Stat(filepath.Join(dir, "test.log.1")); os.IsNotExist(err) {
		t.Error("test.log.1 should exist")
	}

	// test.log.2 should NOT exist (maxFiles=1 means only .1 backup)
	if _, err := os.Stat(filepath.Join(dir, "test.log.2")); err == nil {
		t.Error("test.log.2 should NOT exist when maxFiles=1")
	}
}

func TestLogFileSink_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  500,
		MaxFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				sink.Write(LogEntry{
					Timestamp: "2025-01-01T00:00:00Z",
					Level:     "info",
					Message:   fmt.Sprintf("goroutine-%d-msg-%d", id, i),
				})
			}
		}(g)
	}
	wg.Wait()

	// Should not panic or lose the file handle
	if sink.file == nil {
		t.Error("concurrent writes lost the file handle")
	}
}

func TestLogFileSink_WriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  1024,
		MaxFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	sink.Close()

	// Write after close should not panic
	sink.Write(LogEntry{
		Timestamp: "2025-01-01T00:00:00Z",
		Level:     "info",
		Message:   "after close",
	})
	// If we reach here, no panic - test passes
}

func TestLogFileSink_NestedDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c", "d")

	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  1024,
		MaxFiles: 3,
	})
	if err != nil {
		t.Fatalf("should create nested dirs, got: %v", err)
	}
	defer sink.Close()

	sink.Write(LogEntry{
		Timestamp: "2025-01-01T00:00:00Z",
		Level:     "info",
		Message:   "nested dir test",
	})

	if _, err := os.Stat(filepath.Join(dir, "test.log")); os.IsNotExist(err) {
		t.Error("log file should exist in nested directory")
	}
}

func TestLogFileSink_VeryLargeMessage(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  10 * 1024 * 1024, // 10MB
		MaxFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// 1MB message
	bigMsg := strings.Repeat("X", 1024*1024)
	sink.Write(LogEntry{
		Timestamp: "2025-01-01T00:00:00Z",
		Level:     "info",
		Message:   bigMsg,
	})

	info, err := os.Stat(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 1024*1024 {
		t.Errorf("file should contain >1MB data, got %d", info.Size())
	}
}

func TestLogFileSink_RotatedFilesNoHistory(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  10 * 1024 * 1024,
		MaxFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	files := sink.RotatedFiles()
	if len(files) != 1 {
		t.Errorf("expected 1 file (current only), got %d: %v", len(files), files)
	}
	if files[0] != filepath.Join(dir, "test.log") {
		t.Errorf("first file should be current log, got %s", files[0])
	}
}

func TestLogFileSink_DoubleClose(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  1024,
		MaxFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := sink.Close(); err != nil {
		t.Errorf("first Close should succeed: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("second Close should not error: %v", err)
	}
}

func TestLogFileSink_RotatedFilesAfterMultipleRotations(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewLogFileSink(LogFileSinkConfig{
		Dir:      dir,
		FileName: "test.log",
		MaxSize:  100,
		MaxFiles: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// Write enough to cause several rotations
	for i := 0; i < 200; i++ {
		sink.Write(LogEntry{
			Timestamp: "2025-01-01T00:00:00Z",
			Level:     "info",
			Message:   fmt.Sprintf("rotation-test-msg-%d-pad", i),
		})
	}

	files := sink.RotatedFiles()
	// Should have current + up to maxFiles backups
	if len(files) < 2 {
		t.Errorf("expected at least 2 files after rotation, got %d", len(files))
	}
	if len(files) > 4 { // current + 3 backups max
		t.Errorf("expected at most 4 files (maxFiles=3), got %d", len(files))
	}
}
