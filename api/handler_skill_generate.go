package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hexagon-codes/hexclaw/engine"
)

// GenerateSkillRequest AI 对话创建 Skill 请求。
type GenerateSkillRequest struct {
	Description string `json:"description"` // 用户用自然语言描述想要的技能
}

// skillGenSystemPrompt 指导模型产出一份**完整、合法**的 SKILL.md（frontmatter + 正文）。
// 关键约束：snake_case key、与后端 parseFrontmatter 兼容、icon 用 emoji 或内置 lucide 名，
// 产物仍是标准 SKILL.md（守可移植性，不引入私有插件结构）。
const skillGenSystemPrompt = `你是 HexClaw 的 Skill 作者助手。根据用户描述，生成一份完整、可直接安装的 SKILL.md 文件。

严格输出格式（只输出文件内容本身，不要任何解释、不要 markdown 代码围栏）：

---
name: <英文小写连字符 id，如 weather-report>
description: <一句话描述，供 LLM 理解何时使用>
author: user
version: "1.0.0"
icon: <一个贴切的 emoji，如 📊 / ✍️ / 🔍>
triggers:
  - <触发关键词1>
  - <触发关键词2>
tags:
  - <分类标签1>
  - <分类标签2>
---

# <技能标题>

<技能正文：用 Markdown 写清楚这个技能做什么、如何分步骤完成、注意事项。
这部分会作为系统提示注入对话，要具体、可操作。>

规则：
- name 必须是英文小写+连字符，唯一、无空格。
- 第一行必须是 ---，frontmatter 用 snake_case。
- 不要输出 ` + "```" + ` 代码围栏，直接从 --- 开始。
- 正文用中文（除非用户要求其他语言），结构清晰。`

// handleGenerateSkill 用当前配置的 LLM 把用户的自然语言描述生成为一份 SKILL.md 草稿。
// 仅生成 + 返回原文（不落盘）；前端预览/编辑后再走 POST /skills/install?type=content 安装。
func (s *Server) handleGenerateSkill(w http.ResponseWriter, r *http.Request) {
	var req GenerateSkillRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	desc := strings.TrimSpace(req.Description)
	if desc == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "description 不能为空"})
		return
	}
	if len([]rune(desc)) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "description 过长（上限 2000 字）"})
		return
	}

	eng, ok := s.engine.(*engine.ReActEngine)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "引擎不支持技能生成"})
		return
	}

	content, err := eng.CompleteOnce(r.Context(), skillGenSystemPrompt, desc, 1500)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "生成失败: " + err.Error()})
		return
	}

	content = sanitizeGeneratedSkill(content)
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "模型未返回有效的 SKILL.md（缺少 frontmatter），请重试或调整描述",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"content": content})
}

// sanitizeGeneratedSkill 去掉模型可能误加的 markdown 代码围栏（```markdown ... ``` / ``` ... ```）。
func sanitizeGeneratedSkill(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		// 去掉首行围栏（含可选语言标识）
		if nl := strings.IndexByte(t, '\n'); nl >= 0 {
			t = t[nl+1:]
		}
		t = strings.TrimSuffix(strings.TrimRight(t, "\n"), "```")
	}
	return strings.TrimSpace(t)
}
