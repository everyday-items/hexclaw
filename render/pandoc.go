package render

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// PandocRenderer 调 pandoc subprocess 把 markdown 渲染为目标格式。
//
// 实现要点：
//   - 子进程 stdin 喂 markdown，输出写到受控 temp file（pandoc -o <temp>）。
//     不走 stdout pipe → 避免 stdout 部分写出后再发错误的问题。
//   - PDF 走 --pdf-engine=typst（默认捆绑的 PDF 引擎）。
//   - reader 默认 markdown-raw_html-raw_tex（防 LLM 产物中 <script> / \write18 进入输出）。
//   - 输出超 MaxOutputBytes 立即拒绝。
//   - docx 后处理：仅清空 core.xml 时间戳（避免引入抖动击穿缓存）。
type PandocRenderer struct {
	// PandocPath pandoc 二进制路径；空值时走系统 PATH。
	PandocPath string
	// TypstPath typst 二进制路径；空值时让 pandoc 自己查找。
	TypstPath string
	// SandboxDir 受控临时目录，所有 temp file 在此目录创建。
	SandboxDir string
	// ReferenceDocPath 默认 reference.docx 路径（可选）。
	// 配置后，docx 渲染走 pandoc --reference-doc=<path>，启用统一字体/页边距/标题样式。
	// 资产同时通过 AssetsHash 参与 cache 键，资产改动会让缓存自动失效。
	ReferenceDocPath string
}

// NewPandocRenderer 构造渲染器；SandboxDir 必填，referenceDocPath 可空。
func NewPandocRenderer(pandocPath, typstPath, sandboxDir string) (*PandocRenderer, error) {
	if sandboxDir == "" {
		return nil, errors.New("render: SandboxDir is required")
	}
	if err := os.MkdirAll(sandboxDir, 0o700); err != nil {
		return nil, fmt.Errorf("render: create sandbox dir: %w", err)
	}
	return &PandocRenderer{
		PandocPath: pandocPath,
		TypstPath:  typstPath,
		SandboxDir: sandboxDir,
	}, nil
}

// Render 实现 Renderer 接口。
func (r *PandocRenderer) Render(ctx context.Context, content string, format Format, opts RenderOptions) (*RenderResult, error) {
	if !format.IsValid() {
		return nil, &RenderError{Code: CodeFormatUnsupported, Format: format}
	}

	// 检查 pandoc 可用性
	if _, err := r.lookPandoc(); err != nil {
		return nil, &RenderError{Code: CodeEngineMissing, Format: format, Engine: "pandoc", Detail: err.Error()}
	}

	// PDF 需要 typst（默认捆绑的 PDF 引擎）
	if format == FormatPDF {
		if _, err := r.lookTypst(); err != nil {
			return nil, &RenderError{Code: CodeEngineMissing, Format: format, Engine: "typst", Detail: err.Error()}
		}
	}

	// 创建独占 temp file
	tempPath, err := r.createTempFile(format)
	if err != nil {
		return nil, &RenderError{Code: CodeRenderFailed, Format: format, Detail: err.Error()}
	}
	// 失败路径需要清理 temp file（成功时由 handler 在响应完成后清理）
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = os.Remove(tempPath)
		}
	}()

	// 构造 pandoc 命令
	args := r.buildArgs(format, tempPath, opts)
	pandocBin, _ := r.lookPandoc() // 已在上方校验过

	subCtx, cancel := context.WithTimeout(ctx, timeoutFor(format))
	defer cancel()

	cmd := exec.CommandContext(subCtx, pandocBin, args...)
	cmd.Stdin = strings.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = r.subprocessEnv()
	// pandoc 处理多媒体（image extraction）/ PDF (typst) 时会写 ./media-* 等相对路径产物。
	// 父进程 CWD 在 macOS GUI app 下是 /（只读根），必须把子进程 CWD 钉到 SandboxDir 才能写盘。
	cmd.Dir = r.SandboxDir

	if err := cmd.Run(); err != nil {
		if errors.Is(subCtx.Err(), context.DeadlineExceeded) {
			return nil, &RenderError{Code: CodeTimeout, Format: format, Engine: "pandoc", Detail: truncateStderr(stderr.String())}
		}
		return nil, &RenderError{Code: CodeRenderFailed, Format: format, Engine: "pandoc", Detail: truncateStderr(stderr.String())}
	}

	// 大小检查
	stat, err := os.Stat(tempPath)
	if err != nil {
		return nil, &RenderError{Code: CodeRenderFailed, Format: format, Detail: err.Error()}
	}
	if stat.Size() > MaxOutputBytes {
		return nil, &RenderError{Code: CodeOutputTooLarge, Format: format}
	}

	// docx 后处理：清空 core.xml 时间戳（确定性输出）
	if format == FormatDocx {
		if err := normalizeDocxTimestamps(tempPath); err != nil {
			return nil, &RenderError{Code: CodeRenderFailed, Format: format, Detail: "docx postprocess: " + err.Error()}
		}
		if stat, err = os.Stat(tempPath); err != nil {
			return nil, &RenderError{Code: CodeRenderFailed, Format: format, Detail: err.Error()}
		}
	}

	cleanupOnError = false
	return &RenderResult{
		Path:        tempPath,
		Size:        stat.Size(),
		ContentType: format.MIME(),
		CacheHit:    false,
	}, nil
}

// buildArgs 构造 pandoc 命令行参数。
func (r *PandocRenderer) buildArgs(format Format, outPath string, opts RenderOptions) []string {
	args := []string{}

	// reader：strict markdown，禁 raw HTML / raw TeX（除非 opt-in）
	reader := "markdown"
	if !opts.AllowRawHTML {
		reader += "-raw_html"
	}
	if !opts.AllowRawTeX {
		reader += "-raw_tex"
	}
	// 保留 LLM 输出常用扩展
	reader += "+pipe_tables+task_lists"
	args = append(args, "--from="+reader)

	// writer
	args = append(args, "--to="+writerFor(format))

	// 输出文件（不走 stdout pipe）
	args = append(args, "-o", outPath)

	// PDF 引擎
	if format == FormatPDF {
		args = append(args, "--pdf-engine=typst")
	}

	// HTML 自包含
	if format == FormatHTML {
		args = append(args, "--standalone", "--embed-resources")
	}

	// docx 走默认 reference-doc（统一字体 / 页边距 / 标题样式）
	if format == FormatDocx && r.ReferenceDocPath != "" {
		if _, err := os.Stat(r.ReferenceDocPath); err == nil {
			args = append(args, "--reference-doc="+r.ReferenceDocPath)
		}
	}

	// 元数据：pin 日期防时间戳抖动击穿缓存
	args = append(args, "--metadata=date:19700101")

	// locale 元数据
	locale := opts.effectiveLocale("zh-CN")
	args = append(args, "--metadata=lang:"+locale)

	return args
}

// writerFor 返回 pandoc 的 writer 名称。
func writerFor(format Format) string {
	switch format {
	case FormatMarkdown:
		return "markdown"
	case FormatHTML:
		return "html5"
	case FormatDocx:
		return "docx"
	case FormatPDF:
		return "pdf"
	case FormatEPUB:
		return "epub3"
	case FormatODT:
		return "odt"
	case FormatRTF:
		return "rtf"
	case FormatTXT:
		return "plain"
	default:
		return string(format)
	}
}

// lookPandoc 查找 pandoc 二进制。优先用 PandocPath，否则走系统 PATH。
func (r *PandocRenderer) lookPandoc() (string, error) {
	if r.PandocPath != "" {
		if _, err := os.Stat(r.PandocPath); err == nil {
			return r.PandocPath, nil
		}
	}
	return exec.LookPath("pandoc")
}

func (r *PandocRenderer) lookTypst() (string, error) {
	if r.TypstPath != "" {
		if _, err := os.Stat(r.TypstPath); err == nil {
			return r.TypstPath, nil
		}
	}
	return exec.LookPath("typst")
}

// subprocessEnv 构造子进程环境。仅传 allowlist 中的变量，避免泄露 sidecar 配置。
func (r *PandocRenderer) subprocessEnv() []string {
	allowlist := []string{"LANG", "LC_ALL", "LC_CTYPE", "PATH", "HOME", "TMPDIR", "TYPST_FONT_PATHS"}
	env := []string{}
	for _, k := range allowlist {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// createTempFile 在 sandbox 目录创建唯一 temp file 并返回路径。
func (r *PandocRenderer) createTempFile(format Format) (string, error) {
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idBytes[:])
	name := fmt.Sprintf("render-%s.%s", id, format.Extension())
	path := filepath.Join(r.SandboxDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	return path, nil
}

// docxTimestampPattern 匹配 core.xml 里的 dcterms:created/modified 时间戳。
var docxTimestampPattern = regexp.MustCompile(
	`(<dcterms:(?:created|modified)[^>]*>)([^<]*)(</dcterms:(?:created|modified)>)`)

// docxFixedTime docx zip header 用的固定时间戳（保证两次渲染产物的 zip 元数据一致）。
var docxFixedTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// normalizeDocxTimestamps 把 docx 中 core.xml 的时间戳清空 + zip header 时间戳归零，
// 避免缓存哈希因时间戳抖动而每次失效。
//
// pandoc 默认会把渲染时间写入 dcterms:created/modified；--metadata=date:...
// 只控制可见日期，影响不到 core.xml。
func normalizeDocxTimestamps(path string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}

	// 复制 zip 到 buffer，对 core.xml 做替换
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	for _, f := range r.File {
		err := func() error {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			data, err := io.ReadAll(rc)
			if err != nil {
				return err
			}

			if f.Name == "docProps/core.xml" {
				data = docxTimestampPattern.ReplaceAll(data,
					[]byte("${1}1970-01-01T00:00:00Z${3}"))
			}

			header := &zip.FileHeader{
				Name:     f.Name,
				Method:   f.Method,
				Modified: docxFixedTime,
			}
			fw, err := w.CreateHeader(header)
			if err != nil {
				return err
			}
			_, err = fw.Write(data)
			return err
		}()
		if err != nil {
			_ = r.Close()
			_ = w.Close()
			return err
		}
	}

	_ = r.Close()
	if err := w.Close(); err != nil {
		return err
	}

	// 写回原路径（先写 .new 再 rename，避免半途损坏）
	newPath := path + ".new"
	if err := os.WriteFile(newPath, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(newPath, path)
}
