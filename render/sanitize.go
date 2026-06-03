package render

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// SanitizeFilename 把不可信用户输入（artifact title）清洗成可安全用于
// Content-Disposition 与跨平台文件系统的文件名。
//
// 5 步清洗管线（详见 .claude/doc-generation-architecture.md §3.2）：
//
//  1. 取 basename（剥离任何 / 或 \ 路径前缀）
//  2. 过滤控制字符 0x00-0x1F 与 0x7F（含 CR/LF，防 header injection）
//  3. 过滤跨平台保留字符 < > : " | ? * \\ 与连续点
//  4. 限长 ≤ 200 字符（Windows 路径限制留余量）
//  5. 空字符串 → fallback "artifact-{yyyymmddHHMMSS}.{ext}"
//
// 调用方应同时拿到 ASCII 安全名和 Unicode 名两份，分别用于
// `filename=` 和 `filename*=UTF-8''…`（RFC 5987）。
func SanitizeFilename(rawTitle, ext string) (asciiName, utf8Name string) {
	cleaned := sanitizeBaseName(rawTitle)
	if cleaned == "" {
		cleaned = fmt.Sprintf("artifact-%s", time.Now().Format("20060102150405"))
	}

	// 保证扩展名一致；剥离原扩展（如 title 已带 .md）后统一附加 ext。
	if existingExt := filepath.Ext(cleaned); existingExt != "" {
		// 如果原扩展与目标 ext 一致就保留；否则以目标 ext 为准。
		if strings.EqualFold(strings.TrimPrefix(existingExt, "."), ext) {
			// 已带正确扩展，不变
		} else {
			cleaned = strings.TrimSuffix(cleaned, existingExt) + "." + ext
		}
	} else {
		cleaned = cleaned + "." + ext
	}

	utf8Name = cleaned
	asciiName = toASCII(cleaned)
	return
}

func sanitizeBaseName(s string) string {
	// 1. 取 basename（剥离 / 与 \）
	s = filepath.Base(s)
	// filepath.Base 在 Unix 不处理 \，手动剥
	if i := strings.LastIndexAny(s, `\/`); i >= 0 {
		s = s[i+1:]
	}

	// 2. 过滤控制字符 + 跨平台保留字符
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			continue // 控制字符（含 CR/LF）
		}
		// Windows 保留字符：< > : " | ? * 与反斜杠
		switch r {
		case '<', '>', ':', '"', '|', '?', '*', '\\', '/':
			continue
		}
		b.WriteRune(r)
	}
	s = b.String()

	// 3. 过滤连续点（"a..md" → 留首点；防 ".." 路径攻击 + Windows 不允许尾点）
	s = collapseDots(s)
	s = strings.TrimRight(s, ". ")
	s = strings.TrimLeft(s, " ")

	// 4. 限长（按 rune 数，避免截断 UTF-8 多字节）
	if runes := []rune(s); len(runes) > 200 {
		s = string(runes[:200])
	}

	return s
}

func collapseDots(s string) string {
	var b strings.Builder
	prevDot := false
	for _, r := range s {
		if r == '.' {
			if prevDot {
				continue
			}
			prevDot = true
		} else {
			prevDot = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// toASCII 把非 ASCII 字符（中文 / 维语 / emoji 等）替换为 _，
// 用于不支持 UTF-8 的旧 HTTP 客户端的 filename= 头。
// 现代客户端从 filename*= 取真名。
func toASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > 0x7F || unicode.IsControl(r) {
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ContentDispositionHeader 生成符合 RFC 6266 / RFC 5987 的 Content-Disposition
// 头部值，含 filename=（ASCII fallback）与 filename*=UTF-8''…（Unicode 真名）。
//
// 输入 rawTitle 是不可信用户输入；ext 是格式扩展名（不含点）。
func ContentDispositionHeader(rawTitle, ext string) string {
	asciiName, utf8Name := SanitizeFilename(rawTitle, ext)

	// filename= 的 ASCII 部分内引号转义
	asciiQuoted := strings.ReplaceAll(asciiName, `"`, `\"`)

	// filename*= 走 RFC 5987 percent-encoding
	utf8Encoded := url.PathEscape(utf8Name)

	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		asciiQuoted, utf8Encoded)
}
