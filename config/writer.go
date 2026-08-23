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

// UpsertMCPServer 写入一个已完成 secret metadata 归一化的 MCP server。
// 同名项按一次读-改-写替换，供 Desktop 编辑现有连接时保留/清除 secret。
func (w *Writer) UpsertMCPServer(server MCPServerConfig) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if server.Name == "" {
		return fmt.Errorf("MCP server name cannot be empty")
	}
	// EncryptMCPSecrets 在写盘前会就地替换切片/映射值；复制输入避免把密文
	// 泄漏回 handler 的运行时参数，运行时始终使用解密后的值。
	server.Args = append([]string(nil), server.Args...)
	if server.Env != nil {
		server.Env = cloneMCPStringMapForWriter(server.Env)
	}
	if server.ArgsSecretRefs != nil {
		server.ArgsSecretRefs = cloneMCPArgRefsForWriter(server.ArgsSecretRefs)
	}
	if server.EnvSecretRefs != nil {
		server.EnvSecretRefs = cloneMCPStringMapForWriter(server.EnvSecretRefs)
	}
	cfg, err := w.readConfig()
	if err != nil {
		return err
	}
	found := false
	for i := range cfg.MCP.Servers {
		if cfg.MCP.Servers[i].Name != server.Name {
			continue
		}
		cfg.MCP.Servers[i] = server
		found = true
		break
	}
	if !found {
		cfg.MCP.Servers = append(cfg.MCP.Servers, server)
	}
	return w.writeConfig(cfg)
}

func cloneMCPStringMapForWriter(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func cloneMCPArgRefsForWriter(values map[int]string) map[int]string {
	clone := make(map[int]string, len(values))
	for index, ref := range values {
		clone[index] = ref
	}
	return clone
}

// GetMCPServer 返回指定 server 的解密内存投影；调用方不得把结果写入日志或响应。
func (w *Writer) GetMCPServer(name string) (*MCPServerConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg, err := w.readConfig()
	if err != nil {
		return nil, err
	}
	for i := range cfg.MCP.Servers {
		if cfg.MCP.Servers[i].Name == name {
			server := cfg.MCP.Servers[i]
			return &server, nil
		}
	}
	return nil, nil
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
	// 读改写一致性：把盘上密文 env/secret args 解回明文。Box 缺失或密文损坏时
	// 立即失败，禁止把密文交给 MCP 子进程或通过一次写回静默覆盖。
	if err := DecryptMCPSecrets(cfg.MCP.Servers, w.box); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (w *Writer) writeConfig(cfg *Config) error {
	if err := ensureOwnerOnlyDefaultConfigParent(w.path); err != nil {
		return err
	}
	// 写盘前把 MCP env/secret args 静态加密。新 secret metadata 没有 Box 时
	// fail-closed；无 metadata 的历史普通 MCP 配置保留兼容。
	if err := EncryptMCPSecrets(cfg.MCP.Servers, w.box); err != nil {
		return err
	}
	data, err := marshalConfigForPersistence(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// 仅所有者可访问的 YAML 中可能包含 Provider Key 和 MCP 凭据。
	// 所有写入路径都必须确保整个文件仅所有者可访问。
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
