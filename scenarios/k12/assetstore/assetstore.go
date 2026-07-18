// Package assetstore 是 K12 作品照片的最小本地资产服务（架构设计-v0.5.0 §3.10 作品 /
// §5.5 CreativeWorkVersion.source_asset_id 的真实载体）。
//
// 设计申报（最小可用，边界如下）：
//   - 落盘 <root>/<agent>/<sha256>.<ext>，root 默认 ~/.hexclaw/assets（本机存储，
//     契合 §3.12 隐私承诺「照片仅保存在本机」的桌面直传路径）；HEXCLAW_ASSET_ROOT 可覆盖（测试）。
//   - 内容寻址（sha256）防重复：同字节重复上传幂等返回同一 id，不落重复文件。
//   - 魔数校验：http.DetectContentType 必须探出 image/*（png/jpeg/gif/webp 白名单），
//     伪造扩展名/文本/PDF 一律拒绝；大小上限 10MB。
//   - 归属隔离：路径含 agent（多孩硬边界），读取按 agent 圈定；文件名白名单
//     ^[0-9a-f]{64}\.(png|jpg|gif|webp)$ 杜绝路径穿越。
//   - Asset ID 自描述：asset://<agent>/<sha256>.<ext>——消费方（美术点评视觉链）可离线
//     还原本地路径；归属校验由用例层比对 OwnerOf(id) 与作品 agent。
//   - 不做：远端存储、缩略图派生、引用计数删除（v0.5 最小闭环外，删除随档案删除策略演进）。
package assetstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxAssetBytes 单张作品照片大小上限（10MB）。
const MaxAssetBytes = 10 << 20

// IDPrefix 资产 ID 前缀；与 data:（内联）/本地路径两种既有载体互斥可辨。
const IDPrefix = "asset://"

// extByMIME 图片魔数白名单 → 落盘扩展名。DetectContentType 只认这些常见图片格式，
// 探不出的（如 HEIC）诚实拒绝——宁窄而真。
var extByMIME = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// fileRe 落盘文件名白名单：64 位 sha256 hex + 白名单扩展名。杜绝路径穿越/注入。
var fileRe = regexp.MustCompile(`^[0-9a-f]{64}\.(png|jpg|gif|webp)$`)

// Root 返回资产根目录：HEXCLAW_ASSET_ROOT（测试注入）或 ~/.hexclaw/assets。
func Root() string {
	if v := os.Getenv("HEXCLAW_ASSET_ROOT"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".hexclaw", "assets")
	}
	return filepath.Join(home, ".hexclaw", "assets")
}

// validAgent 校验 agent 名可安全作为目录段（拒绝空/穿越/分隔符/控制字符）。
func validAgent(agent string) error {
	if strings.TrimSpace(agent) == "" {
		return fmt.Errorf("assetstore: agent 不可空")
	}
	if agent == "." || agent == ".." || strings.ContainsAny(agent, `/\`) {
		return fmt.Errorf("assetstore: agent 名含非法路径字符")
	}
	for _, r := range agent {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("assetstore: agent 名含控制字符")
		}
	}
	return nil
}

// Save 校验并落盘一张作品照片，返回自描述资产 ID（asset://<agent>/<sha256>.<ext>）。
// 魔数非 image/* 白名单、超 10MB、agent 不安全一律报错；同内容幂等。
func Save(agent string, data []byte) (string, error) {
	if err := validAgent(agent); err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("assetstore: 图片内容为空")
	}
	if len(data) > MaxAssetBytes {
		return "", fmt.Errorf("assetstore: 图片超过大小上限（%dMB）", MaxAssetBytes>>20)
	}
	mime := http.DetectContentType(data)
	ext, ok := extByMIME[mime]
	if !ok {
		return "", fmt.Errorf("assetstore: 只接受图片文件（png/jpeg/gif/webp），探测到 %s", mime)
	}
	sum := sha256.Sum256(data)
	file := hex.EncodeToString(sum[:]) + "." + ext
	dir := filepath.Join(Root(), agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("assetstore: 建资产目录: %w", err)
	}
	path := filepath.Join(dir, file)
	if _, err := os.Stat(path); err == nil {
		return IDPrefix + agent + "/" + file, nil // 内容寻址幂等：已存在即同一资产
	}
	// 先写临时文件再原子改名，防中断留半文件。
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", fmt.Errorf("assetstore: 写盘: %w", err)
	}
	tmpName := tmp.Name()
	if _, werr := tmp.Write(data); werr != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("assetstore: 写盘: %w", werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("assetstore: 写盘: %w", cerr)
	}
	if rerr := os.Rename(tmpName, path); rerr != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("assetstore: 落盘: %w", rerr)
	}
	return IDPrefix + agent + "/" + file, nil
}

// IsAssetID 判断字符串是否为本服务的资产 ID（区别于 data: 内联与本地路径载体）。
func IsAssetID(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), IDPrefix)
}

// Parse 解析资产 ID → (agent, file)。格式/穿越校验不过即报错。
func Parse(id string) (agent, file string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(id), IDPrefix)
	if !ok {
		return "", "", fmt.Errorf("assetstore: 不是资产 ID: %q", id)
	}
	agent, file, ok = strings.Cut(rest, "/")
	if !ok {
		return "", "", fmt.Errorf("assetstore: 资产 ID 缺少 agent 段")
	}
	if err := validAgent(agent); err != nil {
		return "", "", err
	}
	if !fileRe.MatchString(file) {
		return "", "", fmt.Errorf("assetstore: 资产文件名不合法")
	}
	return agent, file, nil
}

// OwnerOf 返回资产 ID 的归属 agent；非法/非资产 ID 返回 ok=false。
func OwnerOf(id string) (string, bool) {
	agent, _, err := Parse(id)
	if err != nil {
		return "", false
	}
	return agent, true
}

// Read 按 agent + 文件名读回资产（归属隔离：只在该 agent 目录下找；文件名过白名单）。
func Read(agent, file string) (data []byte, mime string, err error) {
	if err := validAgent(agent); err != nil {
		return nil, "", err
	}
	if !fileRe.MatchString(file) {
		return nil, "", fmt.Errorf("assetstore: 资产文件名不合法")
	}
	raw, err := os.ReadFile(filepath.Join(Root(), agent, file))
	if err != nil {
		return nil, "", fmt.Errorf("assetstore: 资产不存在或不可读: %w", err)
	}
	// 双保险：落盘后被替换/损坏时不回非图片字节。
	mime = http.DetectContentType(raw)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", fmt.Errorf("assetstore: 资产内容不是图片（探测到 %s）", mime)
	}
	return raw, mime, nil
}

// PathFromID 把资产 ID 解析为本地文件路径（供美术点评视觉链读原图）。
// 文件必须真实存在；归属校验（id 的 agent == 作品的 agent）由用例层做。
func PathFromID(id string) (string, error) {
	agent, file, err := Parse(id)
	if err != nil {
		return "", err
	}
	path := filepath.Join(Root(), agent, file)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("assetstore: 资产不存在: %w", err)
	}
	return path, nil
}
