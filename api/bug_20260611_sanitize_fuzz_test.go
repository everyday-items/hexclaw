package api

import (
	"errors"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzSanitizeErrorMessage locks the termination invariant fixed in
// BUG-20260611 C1: stripBetween must terminate for any input (the old code
// looped forever when the first line contained " 0x"), must never panic,
// and humanizeError's fallback clip must never split a UTF-8 rune.
func FuzzSanitizeErrorMessage(f *testing.F) {
	f.Add("call failed at 0x14000a2c000 in handler")
	f.Add(" 0x 0x 0x")
	f.Add("plain error message")
	f.Add("失败：上游返回 0xDEADBEEF 地址")
	f.Add("multi\nline\nwith 0x123 stack.go:42: trace")
	f.Add("")
	f.Fuzz(func(t *testing.T, msg string) {
		done := make(chan string, 1)
		go func() {
			done <- humanizeError(errors.New(msg))
		}()
		select {
		case out := <-done:
			if !utf8.ValidString(out) {
				t.Fatalf("humanizeError produced invalid UTF-8 for input %q: %q", msg, out)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("humanizeError did not terminate for input %q", msg)
		}
	})
}
