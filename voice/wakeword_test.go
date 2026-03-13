package voice

import (
	"testing"
)

func TestWakeWordDetector_Detect(t *testing.T) {
	var triggered string
	d := NewWakeWordDetector([]string{"河蟹", "hexclaw"}, func(word, text string) {
		triggered = word
	})

	// 匹配中文唤醒词
	result := d.Detect("你好河蟹，帮我查天气")
	if result != "河蟹" {
		t.Errorf("期望 河蟹, 得到 %q", result)
	}
	if triggered != "河蟹" {
		t.Errorf("回调应被触发，期望 河蟹, 得到 %q", triggered)
	}

	// 匹配英文唤醒词（大小写不敏感）
	triggered = ""
	result = d.Detect("Hey HexClaw, what's up?")
	if result != "hexclaw" {
		t.Errorf("期望 hexclaw, 得到 %q", result)
	}

	// 不匹配
	triggered = ""
	result = d.Detect("今天天气不错")
	if result != "" {
		t.Errorf("期望空, 得到 %q", result)
	}
	if triggered != "" {
		t.Error("不应触发回调")
	}
}

func TestWakeWordDetector_Disabled(t *testing.T) {
	d := NewWakeWordDetector([]string{"河蟹"}, nil)
	d.SetEnabled(false)

	result := d.Detect("河蟹你好")
	if result != "" {
		t.Error("禁用后不应匹配")
	}
}

func TestWakeWordDetector_AddRemoveWord(t *testing.T) {
	d := NewWakeWordDetector([]string{"河蟹"}, nil)

	d.AddWord("小蟹")
	if len(d.Words()) != 2 {
		t.Errorf("期望 2 个唤醒词, 得到 %d", len(d.Words()))
	}

	result := d.Detect("小蟹帮我")
	if result != "小蟹" {
		t.Errorf("期望 小蟹, 得到 %q", result)
	}

	d.RemoveWord("小蟹")
	if len(d.Words()) != 1 {
		t.Errorf("期望 1 个唤醒词, 得到 %d", len(d.Words()))
	}

	result = d.Detect("小蟹帮我")
	if result != "" {
		t.Error("删除后不应匹配")
	}
}

func TestWakeWordDetector_CaseSensitive(t *testing.T) {
	d := NewWakeWordDetector([]string{"HexClaw"}, nil, WakeWithSensitive(true))

	result := d.Detect("hexclaw hello")
	if result != "" {
		t.Error("大小写敏感时 hexclaw 不应匹配 HexClaw")
	}

	result = d.Detect("HexClaw hello")
	if result != "HexClaw" {
		t.Errorf("期望 HexClaw, 得到 %q", result)
	}
}
