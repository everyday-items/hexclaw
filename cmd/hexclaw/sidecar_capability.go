package main

import (
	"fmt"
	"os"
	"strings"
	"unicode"
)

const sidecarCapabilityTokenEnv = "HEXCLAW_SIDECAR_CAPABILITY_TOKEN"

func sidecarCapabilityTokenFromEnv() (string, error) {
	token := strings.TrimSpace(os.Getenv(sidecarCapabilityTokenEnv))
	if token == "" {
		return "", nil
	}
	if len(token) < 32 || len(token) > 512 {
		return "", fmt.Errorf("%s must contain 32-512 bytes", sidecarCapabilityTokenEnv)
	}
	for _, r := range token {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", fmt.Errorf("%s contains invalid whitespace/control characters", sidecarCapabilityTokenEnv)
		}
	}
	return token, nil
}
