package assetstore_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

func TestInspect_ReturnsContentIdentityAndDecodedGeometryForEveryAcceptedFormat(t *testing.T) {
	tests := []struct {
		name      string
		data      func(*testing.T) []byte
		mediaType string
		extension string
		width     int
		height    int
	}{
		{
			name:      "PNG",
			data:      func(t *testing.T) []byte { return encodedImage(t, "png", 3, 2) },
			mediaType: "image/png",
			extension: "png",
			width:     3,
			height:    2,
		},
		{
			name:      "JPEG",
			data:      func(t *testing.T) []byte { return encodedImage(t, "jpeg", 4, 3) },
			mediaType: "image/jpeg",
			extension: "jpg",
			width:     4,
			height:    3,
		},
		{
			name:      "GIF",
			data:      func(t *testing.T) []byte { return encodedImage(t, "gif", 5, 4) },
			mediaType: "image/gif",
			extension: "gif",
			width:     5,
			height:    4,
		},
		{
			name:      "WebP",
			data:      webPFixture,
			mediaType: "image/webp",
			extension: "webp",
			width:     1,
			height:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withRoot(t)
			data := tt.data(t)
			sum := sha256.Sum256(data)
			digest := hex.EncodeToString(sum[:])
			wantID := "asset://mingming/" + digest + "." + tt.extension

			got, err := assetstore.Inspect("mingming", data)
			if err != nil {
				t.Fatalf("Inspect(%s) 失败: %v", tt.name, err)
			}
			if got.AssetID != wantID || got.MediaType != tt.mediaType || got.Extension != tt.extension {
				t.Fatalf("内容身份不一致: got=%+v want id=%q mime=%q ext=%q", got, wantID, tt.mediaType, tt.extension)
			}
			if got.SHA256 != digest || got.SizeBytes != int64(len(data)) {
				t.Fatalf("摘要/大小不一致: got=%+v want sha=%q size=%d", got, digest, len(data))
			}
			if got.PixelWidth != tt.width || got.PixelHeight != tt.height {
				t.Fatalf("解码尺寸不一致: got=%dx%d want=%dx%d", got.PixelWidth, got.PixelHeight, tt.width, tt.height)
			}

			// Describe/Save 必须复用 Inspect 的同一白名单和内容身份，不能产生分叉契约。
			id, mediaType, describedDigest, err := assetstore.Describe("mingming", data)
			if err != nil {
				t.Fatalf("Describe(%s) 失败: %v", tt.name, err)
			}
			if id != got.AssetID || mediaType != got.MediaType || describedDigest != got.SHA256 {
				t.Fatalf("Describe 与 Inspect 分叉: inspect=%+v describe=(%q,%q,%q)", got, id, mediaType, describedDigest)
			}
			savedID, err := assetstore.Save("mingming", data)
			if err != nil {
				t.Fatalf("Save(%s) 失败: %v", tt.name, err)
			}
			if savedID != got.AssetID {
				t.Fatalf("Save 与 Inspect 身份分叉: save=%q inspect=%q", savedID, got.AssetID)
			}
		})
	}
}

func TestInspectAndSave_RejectMalformedAcceptedMagic(t *testing.T) {
	pngData := encodedImage(t, "png", 3, 2)
	webPData := webPFixture(t)
	invalidChecksumPNG, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2n1cAAAAASUVORK5CYII=",
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		data          []byte
		mediaType     string
		configDecodes bool
		configWidth   int
		configHeight  int
		errorContains string
	}{
		{name: "truncated PNG", data: pngData[:24], mediaType: "image/png"},
		{name: "truncated WebP", data: webPData[:20], mediaType: "image/webp"},
		{
			name: "PNG header without pixel data", data: pngConfigOnly(3, 2),
			mediaType: "image/png", configDecodes: true, configWidth: 3, configHeight: 2,
		},
		{
			name: "PNG with invalid zlib checksum", data: invalidChecksumPNG,
			mediaType: "image/png", configDecodes: true, configWidth: 1, configHeight: 1,
			errorContains: "invalid checksum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withRoot(t)
			if got := http.DetectContentType(tt.data); got != tt.mediaType {
				t.Fatalf("测试前提不成立：畸形字节应仍命中白名单魔数，got %q want %q", got, tt.mediaType)
			}
			if tt.configDecodes {
				config, format, err := image.DecodeConfig(bytes.NewReader(tt.data))
				if err != nil || format != "png" ||
					config.Width != tt.configWidth || config.Height != tt.configHeight {
					t.Fatalf("测试前提不成立：畸形 fixture 的配置应可解码，config=%+v format=%q err=%v", config, format, err)
				}
			}
			if _, err := assetstore.Inspect("mingming", tt.data); err == nil {
				t.Fatal("魔数合法但无法完整解码的图片必须拒绝")
			} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
				t.Fatalf("拒绝原因不一致: got %v, want contains %q", err, tt.errorContains)
			}
			if _, err := assetstore.Save("mingming", tt.data); err == nil {
				t.Fatal("Save 必须与 Inspect 一致拒绝畸形图片")
			}
			if _, err := os.Stat(filepath.Join(root, "mingming")); !os.IsNotExist(err) {
				t.Fatalf("拒绝的图片不得创建 agent 资产目录: err=%v", err)
			}
		})
	}
}

func TestInspectAndSave_RejectDecodedPixelLimitBeforePersistence(t *testing.T) {
	root := withRoot(t)
	// 6001*5000 = 30,005,000，刚越过公开的 30MP 防解压炸弹上限。
	data := pngConfigOnly(6001, 5000)
	if got := http.DetectContentType(data); got != "image/png" {
		t.Fatalf("测试前提不成立：构造数据应命中 PNG 魔数，got %q", got)
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatalf("测试前提不成立：超像素 fixture 的配置应可解码: %v", err)
	}
	if _, err := assetstore.Inspect("mingming", data); err == nil {
		t.Fatal("超过像素上限的图片必须拒绝")
	} else if !strings.Contains(err.Error(), "像素上限") {
		t.Fatalf("必须由像素上限而非偶然解码失败拒绝: %v", err)
	}
	if _, err := assetstore.Save("mingming", data); err == nil {
		t.Fatal("Save 必须与 Inspect 一致拒绝超过像素上限的图片")
	}
	if _, err := os.Stat(filepath.Join(root, "mingming")); !os.IsNotExist(err) {
		t.Fatalf("拒绝的图片不得创建 agent 资产目录: err=%v", err)
	}
}

func encodedImage(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	src := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8(20 + x*17),
				G: uint8(40 + y*19),
				B: uint8(60 + (x+y)*11),
				A: 0xff,
			})
		}
	}
	var out bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&out, src)
	case "jpeg":
		err = jpeg.Encode(&out, src, &jpeg.Options{Quality: 90})
	case "gif":
		err = gif.Encode(&out, src, nil)
	default:
		t.Fatalf("未知测试图片格式 %q", format)
	}
	if err != nil {
		t.Fatalf("编码 %s fixture: %v", format, err)
	}
	return out.Bytes()
}

func webPFixture(t *testing.T) []byte {
	t.Helper()
	// 由 libwebp cwebp 生成的 1x1 RGBA WebP；避免测试依赖编码器或外部文件。
	const encoded = "UklGRlgAAABXRUJQVlA4WAoAAAAQAAAAAAAAAAAAQUxQSAIAAAAAf1ZQOCAwAAAA0AEAnQEqAQABAAIANCWgAnS6AfgAA7AA/vDEC/8guWF1yNf/ID/kB/yA//jyAAAA"
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("解码 WebP fixture: %v", err)
	}
	return raw
}

func pngConfigOnly(width, height uint32) []byte {
	out := append([]byte(nil), []byte("\x89PNG\r\n\x1a\n")...)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // truecolor
	return appendPNGChunk(out, "IHDR", ihdr)
}

func appendPNGChunk(dst []byte, kind string, data []byte) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], uint32(len(data)))
	dst = append(dst, encoded[:]...)
	typeAndData := append([]byte(kind), data...)
	dst = append(dst, typeAndData...)
	binary.BigEndian.PutUint32(encoded[:], crc32.ChecksumIEEE(typeAndData))
	return append(dst, encoded[:]...)
}
