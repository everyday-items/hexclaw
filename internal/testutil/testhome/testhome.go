// Package testhome provides process-wide HOME isolation for Go test packages.
package testhome

import (
	"os"
)

// Isolate replaces HOME with a fresh temporary directory and returns a cleanup
// function that restores the original environment before removing that directory.
func Isolate() (func(), error) {
	root, err := os.MkdirTemp("", "hexclaw-test-home-")
	if err != nil {
		return nil, err
	}
	previous, existed := os.LookupEnv("HOME")
	if err := os.Setenv("HOME", root); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return func() {
		if existed {
			_ = os.Setenv("HOME", previous)
		} else {
			_ = os.Unsetenv("HOME")
		}
		_ = os.RemoveAll(root)
	}, nil
}
