package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
)

// HasMessageInput 判断消息是否包含可处理输入。
func HasMessageInput(content string, attachments []Attachment) bool {
	return strings.TrimSpace(content) != "" || len(attachments) > 0
}

// MaxAttachments 单条消息最大附件数（防止 token 爆炸）
const MaxAttachments = 20

// ValidateAttachments 校验附件格式和数量。
func ValidateAttachments(attachments []Attachment) error {
	if len(attachments) > MaxAttachments {
		return fmt.Errorf("一次最多上传 %d 个文件哦，当前选了 %d 个", MaxAttachments, len(attachments))
	}
	for _, attachment := range attachments {
		if attachment.URL == "" && attachment.Data == "" {
			return fmt.Errorf("文件 %s 内容为空，请重新选择", nameOrDefault(attachment.Name))
		}
		if !IsImageAttachment(attachment) {
			return fmt.Errorf("目前仅支持发送图片，文档请先用文字方式粘贴内容。不支持的文件：%s", nameOrDefault(attachment.Name))
		}
	}
	return nil
}

func nameOrDefault(name string) string {
	if name != "" {
		return name
	}
	return "未知文件"
}

// IsImageAttachment 判断附件是否为图片。
func IsImageAttachment(attachment Attachment) bool {
	if strings.EqualFold(attachment.Type, "image") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(attachment.Mime)), "image/")
}

// FilterImageAttachments 过滤出图片附件。
func FilterImageAttachments(attachments []Attachment) []Attachment {
	var images []Attachment
	for _, attachment := range attachments {
		if IsImageAttachment(attachment) {
			images = append(images, attachment)
		}
	}
	return images
}

// BuildUserMessage 按附件情况构建用户消息。
func BuildUserMessage(content string, attachments []Attachment) hexagon.Message {
	imageAttachments := FilterImageAttachments(attachments)
	if len(imageAttachments) == 0 {
		return hexagon.Message{
			Role:    "user",
			Content: content,
		}
	}
	return BuildMultimodalUserMessage(content, imageAttachments)
}

// BuildMultimodalUserMessage 构建包含图片的多模态用户消息。
func BuildMultimodalUserMessage(text string, images []Attachment) hexagon.Message {
	parts := make([]llm.ContentPart, 0, 1+len(images))
	if text != "" {
		parts = append(parts, llm.NewTextPart(text))
	}
	for _, image := range images {
		var imageURL string
		if image.URL != "" {
			imageURL = image.URL
		} else if image.Data != "" {
			mime := image.Mime
			if mime == "" {
				mime = "image/png"
			}
			imageURL = "data:" + mime + ";base64," + image.Data
		}
		if imageURL != "" {
			parts = append(parts, llm.NewImageURLPart(imageURL, "auto"))
		}
	}
	return hexagon.Message{
		Role:         "user",
		MultiContent: parts,
	}
}

// AttachmentCacheKey 构建包含附件摘要的缓存输入键。
func AttachmentCacheKey(content string, attachments []Attachment) string {
	if len(attachments) == 0 {
		return content
	}

	var builder strings.Builder
	builder.WriteString(content)
	for _, attachment := range attachments {
		builder.WriteString("\n[attachment]")
		builder.WriteString(strings.ToLower(strings.TrimSpace(attachment.Type)))
		builder.WriteByte(':')
		builder.WriteString(strings.ToLower(strings.TrimSpace(attachment.Mime)))
		builder.WriteByte(':')

		payload := attachment.URL
		if payload == "" {
			payload = attachment.Data
		}
		sum := sha256.Sum256([]byte(payload))
		builder.WriteString(hex.EncodeToString(sum[:]))
	}
	return builder.String()
}
