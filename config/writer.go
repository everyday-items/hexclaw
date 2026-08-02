package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hexagon-codes/hexclaw/secret"
	"gopkg.in/yaml.v3"
)

// Writer 配置文件读-改-写工具
//
// 支持追加/移除 MCP server 配置到 hexclaw.yaml，
// 保留用户手动编辑的注释和格式。
type Writer struct {
	mu   sync.Mutex
	path string
	box  *secret.Box // 注入后：MCP server 的 env 凭证静态加密落盘（保险箱接管 MCP 凭证）。
}

// NewWriter 创建配置写入器
func NewWriter(path string) *Writer {
	return &Writer{path: path}
}

// SetSecretBox 注入静态加密保险箱。注入后 readConfig 解密、writeConfig 加密 MCP env 凭证；
// 不注入（box==nil）则保持明文（向后兼容既有部署与手编 yaml）。
func (w *Writer) SetSecretBox(box *secret.Box) {
	w.box = box
}

// AppendMCPServer 追加 MCP server 到配置文件。env 为 stdio 进程环境变量（如 DB 凭证），
// 必须一并持久化，否则重启后凭证丢失（连接器拿空环境鉴权失败）。
func (w *Writer) AppendMCPServer(name, transport, command string, args []string, env map[string]string, endpoint string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg, err := w.readConfig()
	if err != nil {
		return err
	}

	// 检查是否已存在
	for _, s := range cfg.MCP.Servers {
		if s.Name == name {
			return fmt.Errorf("MCP server '%s' already exists", name)
		}
	}

	cfg.MCP.Servers = append(cfg.MCP.Servers, MCPServerConfig{
		Name:      name,
		Transport: transport,
		Command:   command,
		Args:      args,
		Env:       env,
		Endpoint:  endpoint,
		Enabled:   true,
	})

	return w.writeConfig(cfg)
}

// RemoveMCPServer 从配置文件移除 MCP server
func (w *Writer) RemoveMCPServer(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg, err := w.readConfig()
	if err != nil {
		return err
	}

	found := false
	servers := make([]MCPServerConfig, 0, len(cfg.MCP.Servers))
	for _, s := range cfg.MCP.Servers {
		if s.Name == name {
			found = true
			continue
		}
		servers = append(servers, s)
	}
	if !found {
		return fmt.Errorf("MCP server '%s' not found", name)
	}

	cfg.MCP.Servers = servers
	return w.writeConfig(cfg)
}

// KnowledgeRetrievalSettings 是检索参数面板（PUT /api/v1/knowledge/config）可调的字段集。
// 与 KnowledgeConfig 的对应字段一一映射；其余知识库配置（enabled / embedding / 分块等）
// 由 UpdateKnowledgeRetrieval 原样保留，不在面板作用域内。
type KnowledgeRetrievalSettings struct {
	Rerank      bool
	RerankModel string
	QueryExpand bool
	Contextual  bool
	MinScore    float64
	CandidateK  int
}

// UpdateKnowledgeRetrieval 持久化检索参数面板字段（读-改-写，保留其余配置段与字段）。
//
// 与 AppendMCPServer 同套读-改-写机制：以 DefaultConfig 为底 overlay 盘上文件，只覆盖
// 检索相关字段后整体写回，避免把文件未提及的段落零值化（见 readConfig 注释）。
func (w *Writer) UpdateKnowledgeRetrieval(s KnowledgeRetrievalSettings) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg, err := w.readConfig()
	if err != nil {
		return err
	}
	cfg.Knowledge.Rerank = s.Rerank
	cfg.Knowledge.RerankModel = s.RerankModel
	cfg.Knowledge.QueryExpand = s.QueryExpand
	cfg.Knowledge.Contextual = s.Contextual
	cfg.Knowledge.MinScore = s.MinScore
	cfg.Knowledge.CandidateK = s.CandidateK
	return w.writeConfig(cfg)
}

// ReadKnowledge 读回当前持久化的知识库配置段（检索参数面板 GET 用，反映最近一次保存值；
// 在 PUT 之前则反映启动时的盘上配置）。
func (w *Writer) ReadKnowledge() (KnowledgeConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg, err := w.readConfig()
	if err != nil {
		return KnowledgeConfig{}, err
	}
	return cfg.Knowledge, nil
}

func (w *Writer) readConfig() (*Config, error) {
	data, err := os.ReadFile(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	// 必须以 DefaultConfig 为底再 overlay 文件内容（与 loader.Load 一致）。
	// 否则用零值 Config 反序列化「精简配置」（如桌面端只写 knowledge.enabled 的文件）时，
	// 文件未提及的段会全部退化为零值；改-写回盘后 mcp.enabled / file_memory.enabled / server.port
	// 等被显式写成 false/0，重启后覆盖 loader 的默认值 → MCP/记忆失效、端口非法启动失败。
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// 读改写一致性：把盘上密文 env 解回明文（无 box 时 no-op）。配合 writeConfig 的加密，
	// 既不双重加密已有 server，也让本次新增的明文 env 在写回时统一加密。
	DecryptMCPEnv(cfg.MCP.Servers, w.box)
	return cfg, nil
}

func (w *Writer) writeConfig(cfg *Config) error {
	// 写盘前把 MCP env 凭证静态加密（无 box 时 no-op，保持明文）。
	EncryptMCPEnv(cfg.MCP.Servers, w.box)
	data, err := marshalConfigForPersistence(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// The config may contain legacy inline provider/MCP credentials. Keep the
	// entire file owner-only even when newer entries use opaque credential refs.
	return ReconcileCommittedWrite(atomicWriteFile(w.path, data, 0o600))
}

type atomicWriteOps struct {
	syncTemp   func(*os.File) error
	replace    func(oldPath, newPath string) error
	syncParent func(dir string) error
}

// PostCommitDurabilityError means the target path was replaced successfully,
// but syncing the parent directory failed. The new bytes are visible to this
// process; their survival across an immediate machine crash is not guaranteed.
//
// Callers must not treat this like a pre-commit failure and blindly write an
// older snapshot back. ReconcileCommittedWrite accepts the outcome only when
// the target was read back and its digest matches the intended bytes.
type PostCommitDurabilityError struct {
	Path           string
	Cause          error
	expectedSHA256 string
	observedSHA256 string
	readbackErr    error
}

func (e *PostCommitDurabilityError) Error() string {
	if e == nil {
		return "post-commit durability error"
	}
	if e.readbackErr != nil {
		return fmt.Sprintf("target replaced but parent directory sync failed: %v (readback failed: %v)", e.Cause, e.readbackErr)
	}
	if !e.ReadbackVerified() {
		return fmt.Sprintf("target replaced but parent directory sync failed: %v (readback digest mismatch)", e.Cause)
	}
	return fmt.Sprintf("target replaced and read back, but parent directory sync failed: %v", e.Cause)
}

func (e *PostCommitDurabilityError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *PostCommitDurabilityError) ReadbackVerified() bool {
	return e != nil && e.readbackErr == nil && e.expectedSHA256 != "" && e.expectedSHA256 == e.observedSHA256
}

func (e *PostCommitDurabilityError) ExpectedSHA256() string {
	if e == nil {
		return ""
	}
	return e.expectedSHA256
}

func (e *PostCommitDurabilityError) ObservedSHA256() string {
	if e == nil {
		return ""
	}
	return e.observedSHA256
}

// ReconcileCommittedWrite converts only a verified post-commit durability
// outcome into success. Ordinary failures and unverified outcomes are retained.
func ReconcileCommittedWrite(err error) error {
	if err == nil {
		return nil
	}
	var committed *PostCommitDurabilityError
	if errors.As(err, &committed) && committed.ReadbackVerified() {
		return nil
	}
	return err
}

// atomicWriteFile 原子且持久地写入文件。临时文件和目标文件位于同一目录，
// 因此 replace 始终发生在同一文件系统内。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return atomicWriteFileWithOps(path, data, perm, atomicWriteOps{
		syncTemp:   (*os.File).Sync,
		replace:    replaceFile,
		syncParent: syncParentDirectory,
	})
}

func atomicWriteFileWithOps(path string, data []byte, perm os.FileMode, ops atomicWriteOps) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".hexclaw-cfg-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	// Apply metadata before Sync so the same durability boundary covers both
	// the bytes and the requested mode.
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := ops.syncTemp(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := ops.replace(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to target: %w", err)
	}
	committed = true
	if err := ops.syncParent(dir); err != nil {
		outcome := &PostCommitDurabilityError{
			Path:           path,
			Cause:          err,
			expectedSHA256: fmt.Sprintf("sha256:%x", sha256.Sum256(data)),
		}
		readback, readErr := os.ReadFile(path)
		if readErr != nil {
			outcome.readbackErr = readErr
		} else {
			outcome.observedSHA256 = fmt.Sprintf("sha256:%x", sha256.Sum256(readback))
		}
		return outcome
	}
	return nil
}
