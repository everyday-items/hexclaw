package api

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestMergeMCPSecretMutationsPreservesEncryptedOwnerValue(t *testing.T) {
	current := &config.MCPServerConfig{
		Name:           "postgres",
		Args:           []string{"-y", "server-postgres", "postgresql://user:old-secret@localhost/db"},
		ArgsSecretRefs: map[int]string{2: "sidecar-connection:v1:connection-1:password"},
		Env:            map[string]string{},
		EnvSecretRefs:  map[string]string{},
	}
	next, err := mergeMCPSecretMutations(current, config.MCPServerConfig{
		Name: "postgres",
		Args: []string{"-y", "server-postgres", "postgresql://user@db2.example/db"},
	}, []mcpSecretArgMutation{{Index: 2, Mode: "preserve", CredentialRef: "sidecar-connection:v1:connection-1:password"}}, nil)
	if err != nil {
		t.Fatalf("merge preserve: %v", err)
	}
	if next.Args[2] != "postgresql://user:old-secret@db2.example/db" {
		t.Fatalf("preserve must reuse the secret while applying non-secret edits, got %q", next.Args[2])
	}
	if next.ArgsSecretRefs[2] != current.ArgsSecretRefs[2] {
		t.Fatalf("preserve lost secret reference: %#v", next.ArgsSecretRefs)
	}
}

func TestMergeMCPSecretMutationsClearsEnvSecretWithoutPlaintext(t *testing.T) {
	current := &config.MCPServerConfig{
		Name:          "mysql",
		Env:           map[string]string{"MYSQL_PASS": "old-secret", "MYSQL_HOST": "localhost"},
		EnvSecretRefs: map[string]string{"MYSQL_PASS": "sidecar-connection:v1:connection-1:password"},
	}
	next, err := mergeMCPSecretMutations(current, config.MCPServerConfig{
		Name: "mysql",
		Env:  map[string]string{"MYSQL_HOST": "localhost"},
	}, nil, []mcpSecretEnvMutation{{Key: "MYSQL_PASS", Mode: "clear", CredentialRef: "sidecar-connection:v1:connection-1:password"}})
	if err != nil {
		t.Fatalf("merge clear: %v", err)
	}
	if _, ok := next.Env["MYSQL_PASS"]; ok {
		t.Fatalf("clear must not retain MYSQL_PASS: %#v", next.Env)
	}
	if _, ok := next.EnvSecretRefs["MYSQL_PASS"]; ok {
		t.Fatalf("clear must remove secret reference: %#v", next.EnvSecretRefs)
	}
}

func TestMergeMCPSecretMutationsRejectsPreserveWithoutOwnerReference(t *testing.T) {
	_, err := mergeMCPSecretMutations(&config.MCPServerConfig{
		Name: "mysql",
		Env:  map[string]string{"MYSQL_PASS": "old-secret"},
	}, config.MCPServerConfig{
		Name: "mysql",
		Env:  map[string]string{},
	}, nil, []mcpSecretEnvMutation{{Key: "MYSQL_PASS", Mode: "preserve"}})
	if err == nil || !strings.Contains(err.Error(), "credential reference") {
		t.Fatalf("preserve without reference must fail closed, got %v", err)
	}
}
