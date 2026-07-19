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
//   - 删除：按 agent 整体抹除（DeleteAgent，Agent 注销级联，见 §3.12 隐私对偶）；
//     不做远端存储、缩略图派生、单资产引用计数删除（v0.5 最小闭环外）。
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
	id, _, err := Ensure(agent, data)
	return id, err
}

// Describe 只校验资产内容并计算其内容寻址身份，不写文件。它是 .hexbak v3
// manifest 校验与 restore-as 预检的单一真相源。
func Describe(agent string, data []byte) (id, mime, digest string, err error) {
	if err := validAgent(agent); err != nil {
		return "", "", "", err
	}
	if len(data) == 0 {
		return "", "", "", fmt.Errorf("assetstore: 图片内容为空")
	}
	if len(data) > MaxAssetBytes {
		return "", "", "", fmt.Errorf("assetstore: 图片超过大小上限（%dMB）", MaxAssetBytes>>20)
	}
	mime = http.DetectContentType(data)
	ext, ok := extByMIME[mime]
	if !ok {
		return "", "", "", fmt.Errorf("assetstore: 只接受图片文件（png/jpeg/gif/webp），探测到 %s", mime)
	}
	sum := sha256.Sum256(data)
	digest = hex.EncodeToString(sum[:])
	file := digest + "." + ext
	return IDPrefix + agent + "/" + file, mime, digest, nil
}

// Ensure 将内容寻址资产写入 agent 作用域，并返回本次是否新建了最终文件。
// 最终路径以 hard-link 从同目录临时文件原子发布；并发命中同一内容时只有一个调用
// 返回 created=true，其余调用验证现存字节后幂等复用。
func Ensure(agent string, data []byte) (id string, created bool, err error) {
	id, _, digest, err := Describe(agent, data)
	if err != nil {
		return "", false, err
	}
	_, file, err := Parse(id)
	if err != nil {
		return "", false, err
	}
	dir := filepath.Join(Root(), agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, fmt.Errorf("assetstore: 建资产目录: %w", err)
	}
	path := filepath.Join(dir, file)
	if _, err := os.Stat(path); err == nil {
		if err := verifyExistingContent(path, digest); err != nil {
			return "", false, err
		}
		return id, false, nil
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("assetstore: 检查目标资产: %w", err)
	}
	// 先写临时文件再以 hard-link 原子发布，防中断留半文件。
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", false, fmt.Errorf("assetstore: 写盘: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, werr := tmp.Write(data); werr != nil {
		tmp.Close()
		return "", false, fmt.Errorf("assetstore: 写盘: %w", werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return "", false, fmt.Errorf("assetstore: 写盘: %w", cerr)
	}
	if lerr := os.Link(tmpName, path); lerr != nil {
		if os.IsExist(lerr) {
			if err := verifyExistingContent(path, digest); err != nil {
				return "", false, err
			}
			return id, false, nil
		}
		return "", false, fmt.Errorf("assetstore: 落盘: %w", lerr)
	}
	return id, true, nil
}

func verifyExistingContent(path, wantDigest string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("assetstore: 校验现存资产: %w", err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != wantDigest {
		return fmt.Errorf("assetstore: 内容寻址资产校验失败")
	}
	return nil
}

// Remove 删除一个明确归属于 agent 的内容寻址资产。它只删除解析后的单一白名单文件；
// 不存在视为幂等成功。返回本次是否实际删除，供 restore-as 补偿/回滚取证。
func Remove(agent, id string) (bool, error) {
	if err := validAgent(agent); err != nil {
		return false, err
	}
	owner, file, err := Parse(id)
	if err != nil {
		return false, err
	}
	if owner != agent {
		return false, fmt.Errorf("assetstore: 资产不属于目标 agent")
	}
	dir := filepath.Join(Root(), agent)
	err = os.Remove(filepath.Join(dir, file))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("assetstore: 删除资产: %w", err)
	}
	// 补偿删除最后一个文件时顺手回收空 agent 目录；目录非空/并发新增时忽略。
	_ = os.Remove(dir)
	return true, nil
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

// DeleteAgent 抹除某 agent 名下全部作品资产文件与目录（Agent 注销级联清理，架构 §3.12
// 「照片仅本机」承诺的对偶：孩子档案删除时本机照片一并抹除，不留残影）。
//
// 归属校验：agent 名过 validAgent 白名单（拒绝空/穿越/分隔符/控制字符），杜绝把删除
// 落到根目录或错误路径。目录不存在视为已清（幂等，返回 0, nil）。返回被删除的合法资产
// 文件数（供审计/回归断言；不含临时文件与非白名单文件）。
func DeleteAgent(agent string) (int, error) {
	if err := validAgent(agent); err != nil {
		return 0, err
	}
	dir := filepath.Join(Root(), agent)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("assetstore: 读取资产目录: %w", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && fileRe.MatchString(e.Name()) {
			n++
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return 0, fmt.Errorf("assetstore: 删除资产目录: %w", err)
	}
	return n, nil
}

// AgentAssets 是某 agent 全部资产的进程内字节快照，作为 Agent 注销 saga 的补偿载体：
// 删前留存，后续归属删除（Agent 行删）失败时可原样回填。仅在删除请求期间存活，注销
// 成功后随请求 GC——不落任何额外磁盘残留，也无需提交后钩子。
type AgentAssets struct {
	agent string
	files map[string][]byte
}

// SnapshotAgent 读取该 agent 目录下全部合法资产文件为内存快照（注销回滚补偿）。
// 目录不存在返回空快照（Restore 为 no-op）。
func SnapshotAgent(agent string) (*AgentAssets, error) {
	if err := validAgent(agent); err != nil {
		return nil, err
	}
	snap := &AgentAssets{agent: agent, files: map[string][]byte{}}
	dir := filepath.Join(Root(), agent)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return snap, nil
		}
		return nil, fmt.Errorf("assetstore: 快照资产目录: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !fileRe.MatchString(e.Name()) {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			return nil, fmt.Errorf("assetstore: 快照读取 %s: %w", e.Name(), rerr)
		}
		snap.files[e.Name()] = data
	}
	return snap, nil
}

// Restore 从快照重建 agent 资产（注销回滚补偿）。内容寻址重写幂等；空快照 no-op。
func (a *AgentAssets) Restore() error {
	if a == nil || len(a.files) == 0 {
		return nil
	}
	dir := filepath.Join(Root(), a.agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("assetstore: 回滚重建目录: %w", err)
	}
	for name, data := range a.files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return fmt.Errorf("assetstore: 回滚重写 %s: %w", name, err)
		}
	}
	return nil
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
