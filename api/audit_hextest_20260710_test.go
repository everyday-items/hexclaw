package api

// hex-test 全生态审计 RED 取证（2026-07-10）——契约面 🔴#1。
// 症状：hexclaw-desktop chat.ts:174 + Rust 桥 commands.rs:371-375 认真下发
// temperature / max_tokens，但后端 ChatRequest(server.go:954) 无对应 json 承载字段，
// json.Decode 静默丢弃 → 用户在 UI 设的温度/最大长度全链失效（HTTP + WS 两条入口）。
// 该测试现作为契约回归：ChatRequest 必须继续承载这两个结构化字段；端到端透传、
// 校验和请求级优先级另由 server_test / adapter/web / engine 回归覆盖。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAudit20260710_ChatRequestDropsSamplingParams(t *testing.T) {
	body := `{"message":"hi","temperature":0.2,"max_tokens":128}`
	var req ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rt := reflect.TypeOf(req)
	hasTemp, hasMax := false, false
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		switch name {
		case "temperature":
			hasTemp = true
		case "max_tokens":
			hasMax = true
		}
	}

	if !hasTemp || !hasMax {
		t.Fatalf("RED 契约面#1: ChatRequest 缺采样参数承载字段 (temperature=%v max_tokens=%v) "+
			"→ 前端 chat.ts:174 下发的 temperature/max_tokens 被 json.Decode 静默丢弃, "+
			"UI 温度/最大长度设置全链失效", hasTemp, hasMax)
	}
}
