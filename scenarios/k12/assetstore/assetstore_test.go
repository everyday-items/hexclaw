package assetstore_test

// 最小资产服务契约（RED 先行）——K12 作品照片真实上传（架构设计-v0.5.0 §3.10 作品 / §5.5
// CreativeWorkVersion.source_asset_id 的落地载体，设计申报见包注释）：
//   1. 魔数校验：只收真实 image/*（png/jpeg/gif/webp），伪造扩展名/文本一律拒绝；
//   2. 大小上限：>10MB 拒绝；
//   3. 内容寻址：同字节重复上传返回同一 asset id，不落重复文件；
//   4. 归属隔离：路径含 agent；按 agent 解析他人资产必须失败（含路径穿越）；
//   5. ID 自描述：asset://<agent>/<sha256>.<ext>，Parse/OwnerOf 可还原归属。

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

// tinyPNG 1×1 真实 PNG（魔数合法）。
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	const b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func withRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HEXCLAW_ASSET_ROOT", root)
	return root
}

func TestSave_RoundtripAndContentAddressing(t *testing.T) {
	root := withRoot(t)
	png := tinyPNG(t)
	id, err := assetstore.Save("mingming", png)
	if err != nil {
		t.Fatalf("保存合法 PNG 失败: %v", err)
	}
	if !strings.HasPrefix(id, "asset://mingming/") || !strings.HasSuffix(id, ".png") {
		t.Fatalf("asset id 应为 asset://mingming/<sha256>.png, got %q", id)
	}
	// 内容寻址：重复上传同字节 → 同 id，磁盘只有一个文件。
	id2, err := assetstore.Save("mingming", png)
	if err != nil || id2 != id {
		t.Fatalf("同内容重复上传应幂等同 id: %q vs %q (err=%v)", id, id2, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "mingming"))
	if len(entries) != 1 {
		t.Fatalf("内容寻址应只落一个文件, got %d", len(entries))
	}
	// 读回：字节一致 + mime 正确。
	owner, file, err := assetstore.Parse(id)
	if err != nil || owner != "mingming" {
		t.Fatalf("Parse 失败: owner=%q err=%v", owner, err)
	}
	data, mime, err := assetstore.Read("mingming", file)
	if err != nil || !bytes.Equal(data, png) || mime != "image/png" {
		t.Fatalf("读回不一致: mime=%q err=%v", mime, err)
	}
}

func TestSave_RejectsNonImageMagic(t *testing.T) {
	withRoot(t)
	if _, err := assetstore.Save("mingming", []byte("plain text pretending .png")); err == nil {
		t.Fatal("非图片字节（魔数不符）必须拒绝")
	}
	// PDF 魔数也不是 image/*。
	if _, err := assetstore.Save("mingming", []byte("%PDF-1.4 fake")); err == nil {
		t.Fatal("PDF 魔数必须拒绝")
	}
}

func TestSave_RejectsOversize(t *testing.T) {
	withRoot(t)
	big := make([]byte, assetstore.MaxAssetBytes+1)
	copy(big, tinyPNG(t)) // 魔数合法但超限
	if _, err := assetstore.Save("mingming", big); err == nil {
		t.Fatal(">10MB 必须拒绝")
	}
}

func TestSave_RejectsUnsafeAgent(t *testing.T) {
	withRoot(t)
	for _, agent := range []string{"", "..", "a/b", `a\b`, "a/../b"} {
		if _, err := assetstore.Save(agent, tinyPNG(t)); err == nil {
			t.Fatalf("不安全 agent %q 必须拒绝", agent)
		}
	}
}

func TestRead_OwnershipIsolationAndTraversal(t *testing.T) {
	withRoot(t)
	id, err := assetstore.Save("mingming", tinyPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	_, file, _ := assetstore.Parse(id)
	// 归属隔离：别的 agent 读不到（路径含 agent，读取按 agent 圈定）。
	if _, _, err := assetstore.Read("honghong", file); err == nil {
		t.Fatal("跨 agent 读取必须失败")
	}
	// 路径穿越：文件名不满足 <sha256>.<ext> 白名单一律拒绝。
	for _, evil := range []string{"../mingming/" + file, "..%2f" + file, "x.png", strings.Repeat("a", 64) + ".sh"} {
		if _, _, err := assetstore.Read("honghong", evil); err == nil {
			t.Fatalf("非法文件名 %q 必须拒绝", evil)
		}
	}
}

func TestParse_And_OwnerOf(t *testing.T) {
	if _, _, err := assetstore.Parse("asset://a/../b.png"); err == nil {
		t.Fatal("穿越型 id 必须拒绝")
	}
	if _, ok := assetstore.OwnerOf("not-an-asset"); ok {
		t.Fatal("非 asset id 不应有 owner")
	}
	if !assetstore.IsAssetID("asset://mingming/abc.png") {
		t.Fatal("asset:// 前缀应被识别")
	}
	if assetstore.IsAssetID("data:image/png;base64,xxx") || assetstore.IsAssetID("/tmp/x.png") {
		t.Fatal("data URL / 本地路径不是 asset id")
	}
}

func TestPathFromID_ResolvesUnderRoot(t *testing.T) {
	root := withRoot(t)
	id, err := assetstore.Save("mingming", tinyPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	p, err := assetstore.PathFromID(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, root) {
		t.Fatalf("解析路径应位于资产根内: %q", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("解析路径应真实存在: %v", err)
	}
	if _, err := assetstore.PathFromID("asset://mingming/deadbeef.png"); err == nil {
		t.Fatal("不存在/非法哈希名的 id 应报错")
	}
}
