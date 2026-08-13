package main

import "testing"

func TestVersionCommandRetainsSidecarVersionIdentity(t *testing.T) {
	command := newVersionCmd()
	if got := command.Annotations[sidecarVersionIdentityAnnotation]; got != sidecarVersionIdentity {
		t.Fatalf("sidecar version identity annotation = %q, want %q", got, sidecarVersionIdentity)
	}
}
