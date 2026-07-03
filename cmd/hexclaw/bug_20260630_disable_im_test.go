package main

import (
	"reflect"
	"testing"
)

func TestBug20260630_ParseDisabledIMProviders(t *testing.T) {
	if got := parseDisabledIMProviders(""); len(got) != 0 {
		t.Fatalf("empty should disable none, got %v", got)
	}
	if got := parseDisabledIMProviders("dingtalk, feishu dingtalk"); !reflect.DeepEqual(got, []string{"dingtalk", "feishu"}) {
		t.Fatalf("dedup/sort failed: %v", got)
	}
	all := parseDisabledIMProviders("all")
	if len(all) < 6 {
		t.Fatalf("all should cover IM providers, got %v", all)
	}
}
