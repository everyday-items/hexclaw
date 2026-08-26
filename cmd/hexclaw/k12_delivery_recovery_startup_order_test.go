package main

import (
	"bytes"
	"os"
	"testing"
)

func TestK12DeliveryRecoveryStartsAfterEnabledInstances(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	startEnabled := bytes.Index(source, []byte("if err := instanceMgr.StartEnabled(ctx); err != nil"))
	recoverReceipts := bytes.Index(source, []byte("RecoverDeliveryReceipts(ctx, agent.Name)"))
	if startEnabled < 0 || recoverReceipts < 0 {
		t.Fatalf("startup contract anchors missing: StartEnabled=%d RecoverDeliveryReceipts=%d", startEnabled, recoverReceipts)
	}
	if recoverReceipts < startEnabled {
		t.Fatalf("delivery receipt recovery must start after enabled instances: StartEnabled=%d RecoverDeliveryReceipts=%d", startEnabled, recoverReceipts)
	}
}
