package voice

import (
	"strings"
	"sync"
)

// WakeWordDetector 语音唤醒词检测器
//
// 支持关键词检测，当 STT 转录文本中包含唤醒词时触发。
// 采用文本匹配方式，在 STT 输出后检测，无需专用唤醒模型。
type WakeWordDetector struct {
	mu        sync.RWMutex
	words     []string
	callback  WakeCallback
	enabled   bool
	sensitive bool // 是否区分大小写
}

// WakeCallback 唤醒回调
type WakeCallback func(word string, text string)

// WakeWordOption 配置选项
type WakeWordOption func(*WakeWordDetector)

// WakeWithSensitive 设置大小写敏感
func WakeWithSensitive(sensitive bool) WakeWordOption {
	return func(d *WakeWordDetector) { d.sensitive = sensitive }
}

// NewWakeWordDetector 创建唤醒词检测器
//
// words: 唤醒词列表（如 "河蟹", "hexclaw", "小蟹"）
// callback: 检测到唤醒词时的回调
func NewWakeWordDetector(words []string, callback WakeCallback, opts ...WakeWordOption) *WakeWordDetector {
	d := &WakeWordDetector{
		words:    words,
		callback: callback,
		enabled:  true,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Detect 检测文本中是否包含唤醒词
//
// 返回匹配到的唤醒词，未匹配返回空字符串。
func (d *WakeWordDetector) Detect(text string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if !d.enabled || len(d.words) == 0 {
		return ""
	}

	checkText := text
	if !d.sensitive {
		checkText = strings.ToLower(text)
	}

	for _, word := range d.words {
		checkWord := word
		if !d.sensitive {
			checkWord = strings.ToLower(word)
		}
		if strings.Contains(checkText, checkWord) {
			if d.callback != nil {
				d.callback(word, text)
			}
			return word
		}
	}
	return ""
}

// SetEnabled 启用/禁用检测
func (d *WakeWordDetector) SetEnabled(enabled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.enabled = enabled
}

// AddWord 添加唤醒词
func (d *WakeWordDetector) AddWord(word string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.words = append(d.words, word)
}

// RemoveWord 移除唤醒词
func (d *WakeWordDetector) RemoveWord(word string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, w := range d.words {
		if w == word {
			d.words = append(d.words[:i], d.words[i+1:]...)
			return
		}
	}
}

// Words 返回当前唤醒词列表
func (d *WakeWordDetector) Words() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make([]string, len(d.words))
	copy(result, d.words)
	return result
}
