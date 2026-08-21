package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

const bug20260808InstalledAppExecutable = "HEXCLAW_INSTALLED_APP_EXECUTABLE"

// TestBUG20260808YAMLProviderKeySurvivesRestart_RealHexClawGPT 通过 owner YAML 的
// 隔离副本验证本机实际配置的 HexClaw-GPT Provider 当前模型，且不会记录端点、Provider 密钥或其摘要。
func TestBUG20260808YAMLProviderKeySurvivesRestart_RealHexClawGPT(t *testing.T) {
	if strings.TrimSpace(os.Getenv(bug20260728LiveProviderProbeGate)) != "1" {
		t.Skip("set HEXCLAW_REAL_PROVIDER_PROBE=1 to run the real HexClaw-GPT persistence probe")
	}

	source, err := config.Load(bug20260728DesktopConfigPath(t))
	if err != nil {
		t.Fatal("load local HexClaw configuration failed (details withheld to protect credentials)")
	}
	providerKey, providerInstanceID := bug20260728FindLiveProvider(t, source)
	provider := source.LLM.Providers[providerKey]
	if strings.TrimSpace(provider.APIKey) == "" {
		t.Fatal("the local HexClaw-GPT provider has no YAML API key to persist")
	}
	sourceDigest := sha256.Sum256([]byte(provider.APIKey))

	configPath := filepath.Join(t.TempDir(), "isolated-home", ".hexclaw", "hexclaw.yaml")
	if saveErr := config.Save(source, configPath); saveErr != nil {
		t.Fatal("persist isolated provider configuration failed")
	}
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("isolated YAML permission=%#o, want 0600", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("isolated YAML directory permission=%#o, want 0700", directoryInfo.Mode().Perm())
	}

	restartedConfig, err := config.Load(configPath)
	if err != nil {
		t.Fatal("reload isolated provider configuration failed")
	}
	restartedProvider := restartedConfig.LLM.Providers[providerKey]
	if sha256.Sum256([]byte(restartedProvider.APIKey)) != sourceDigest {
		t.Fatal("provider API key changed or disappeared across isolated YAML restart")
	}

	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "yaml-provider-persistence-live.db"))
	if err != nil {
		t.Fatalf("open isolated receipt store: error_type=%T", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(t.Context()); err != nil {
		t.Fatalf("init isolated receipt store: error_type=%T", err)
	}

	restarted := &Server{cfg: restartedConfig, store: store}
	probe := bug20260728RunLiveProviderProbe(t, restarted, providerInstanceID)
	if ok, _ := probe["ok"].(bool); !ok {
		t.Fatal("real HexClaw-GPT probe did not succeed after YAML restart")
	}
	expectedModel := strings.TrimSpace(provider.Model)
	if model, _ := probe["model"].(string); model != expectedModel {
		t.Fatalf("real YAML persistence probe model=%q, want configured model %q", model, expectedModel)
	}

	t.Logf("REAL_YAML_PROVIDER_PERSISTENCE_PASS provider=HexClaw-GPT model=%s latency_ms=%.0f", expectedModel, probe["latency_ms"])
}

// TestBUG20260808UnsignedInstalledAppPreservesOwnerYAML_RealHexClawGPT 使用同一份
// 仅 owner 可访问的隔离 YAML，将刚构建且未签名的 Desktop 可执行文件运行两次。
// app/Sidecar 生命周期为真实链路；最终 Provider 探针使用同一份重新加载的 YAML 与
// 生产连接测试链路。测试不会打印端点、凭据、摘要或子进程日志。
func TestBUG20260808UnsignedInstalledAppPreservesOwnerYAML_RealHexClawGPT(t *testing.T) {
	if strings.TrimSpace(os.Getenv(bug20260728LiveProviderProbeGate)) != "1" {
		t.Skip("set HEXCLAW_REAL_PROVIDER_PROBE=1 to run the real installed-app persistence probe")
	}
	appExecutable := strings.TrimSpace(os.Getenv(bug20260808InstalledAppExecutable))
	if !filepath.IsAbs(appExecutable) {
		t.Fatalf("%s must name an absolute freshly built Desktop executable", bug20260808InstalledAppExecutable)
	}
	if info, err := os.Stat(appExecutable); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("%s must name an executable file", bug20260808InstalledAppExecutable)
	}

	source, err := config.Load(bug20260728DesktopConfigPath(t))
	if err != nil {
		t.Fatal("load local HexClaw configuration failed (details withheld to protect credentials)")
	}
	providerKey, providerInstanceID := bug20260728FindLiveProvider(t, source)
	provider := source.LLM.Providers[providerKey]
	if strings.TrimSpace(provider.APIKey) == "" {
		t.Fatal("the local HexClaw-GPT provider has no YAML API key to persist")
	}
	sourceDigest := sha256.Sum256([]byte(provider.APIKey))

	port := bug20260808FreeLoopbackPort(t)
	runRoot, err := os.MkdirTemp("", "hexclaw-bug015-installed-app-")
	if err != nil {
		t.Fatal("create isolated installed-app test root failed")
	}
	if chmodErr := os.Chmod(runRoot, 0o700); chmodErr != nil {
		t.Fatal("protect isolated installed-app test root failed")
	}
	t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
	home := filepath.Join(runRoot, "installed-home")
	configPath := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	if mkdirErr := os.MkdirAll(filepath.Dir(configPath), 0o700); mkdirErr != nil {
		t.Fatal("create isolated installed-app configuration directory failed")
	}
	if chmodErr := os.Chmod(home, 0o700); chmodErr != nil {
		t.Fatal("protect isolated installed-app home failed")
	}
	if chmodErr := os.Chmod(filepath.Dir(configPath), 0o700); chmodErr != nil {
		t.Fatal("protect isolated installed-app configuration directory failed")
	}

	isolated := config.DefaultConfig()
	isolated.Server = config.ServerConfig{Host: "127.0.0.1", Port: port, MCPPort: 16070, Mode: "production"}
	isolated.LLM = config.LLMConfig{
		Default:   providerKey,
		Providers: map[string]config.LLMProviderConfig{providerKey: provider},
		Routing:   config.LLMRoutingConfig{Enabled: false},
		Cache:     config.LLMCacheConfig{Enabled: false},
		Tools:     config.LLMToolsConfig{Enabled: "off"},
	}
	isolated.Platforms = config.PlatformsConfig{}
	isolated.Skill = config.SkillConfig{}
	isolated.Storage = config.StorageConfig{
		Driver: "sqlite",
		SQLite: config.SQLiteConfig{Path: filepath.Join(home, ".hexclaw", "data.db")},
	}
	isolated.Memory = config.MemoryConfig{}
	isolated.Knowledge = config.KnowledgeConfig{}
	isolated.MCP = config.MCPConfig{}
	isolated.Skills = config.SkillsConfig{}
	isolated.Heartbeat = config.HeartbeatConfig{}
	isolated.Cron = config.CronConfig{}
	isolated.Webhook = config.WebhookConfig{}
	isolated.Compaction = config.CompactionConfig{}
	isolated.FileMemory = config.FileMemoryConfig{}
	isolated.Router = config.RouterConfig{}
	isolated.Canvas = config.CanvasConfig{}
	isolated.Audit = config.AuditConfig{}
	isolated.Voice = config.VoiceConfig{}
	if saveErr := config.Save(isolated, configPath); saveErr != nil {
		t.Fatal("save isolated installed-app Provider YAML failed")
	}
	bug20260808AssertOwnerOnlyYAML(t, configPath)

	securityAgentBefore := bug20260808SecurityAgentPIDs(t.Context())
	for restart := 1; restart <= 2; restart++ {
		bug20260808RunInstalledAppOnce(t, appExecutable, runRoot, home, port, restart)
		reloaded, loadErr := config.Load(configPath)
		if loadErr != nil {
			t.Fatal("reload installed-app YAML failed (details withheld to protect credentials)")
		}
		if sha256.Sum256([]byte(reloaded.LLM.Providers[providerKey].APIKey)) != sourceDigest {
			t.Fatalf("installed-app restart %d changed or removed the Provider YAML key", restart)
		}
		bug20260808AssertOwnerOnlyYAML(t, configPath)
	}
	if added := bug20260808StringSetDifference(bug20260808SecurityAgentPIDs(t.Context()), securityAgentBefore); len(added) != 0 {
		t.Fatalf("installed-app run started a new macOS SecurityAgent process: count=%d", len(added))
	}
	bug20260808AssertSingleSecretCopy(t, runRoot, configPath, []byte(provider.APIKey))

	restartedConfig, err := config.Load(configPath)
	if err != nil {
		t.Fatal("reload installed-app Provider YAML for real probe failed")
	}
	store, err := sqlitestore.New(filepath.Join(runRoot, "installed-app-provider-probe.db"))
	if err != nil {
		t.Fatalf("open installed-app probe receipt store: error_type=%T", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(t.Context()); err != nil {
		t.Fatalf("init installed-app probe receipt store: error_type=%T", err)
	}
	probe := bug20260728RunLiveProviderProbe(t, &Server{cfg: restartedConfig, store: store}, providerInstanceID)
	if ok, _ := probe["ok"].(bool); !ok {
		t.Fatal("real HexClaw-GPT probe failed after installed App restart")
	}
	expectedModel := strings.TrimSpace(provider.Model)
	if model, _ := probe["model"].(string); model != expectedModel {
		t.Fatalf("installed-app YAML probe model=%q, want configured model %q", model, expectedModel)
	}
	t.Logf("REAL_UNSIGNED_INSTALLED_APP_YAML_PASS restarts=2 provider=HexClaw-GPT model=%s latency_ms=%.0f", expectedModel, probe["latency_ms"])
}

func bug20260808FreeLoopbackPort(t *testing.T) int {
	t.Helper()
	var listenerConfig net.ListenConfig
	listener, err := listenerConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("allocate isolated installed-app port failed")
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func bug20260808RunInstalledAppOnce(
	t *testing.T,
	appExecutable string,
	runRoot string,
	home string,
	port int,
	restart int,
) {
	t.Helper()
	processCtx, cancelProcess := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancelProcess()

	logPath := filepath.Join(runRoot, fmt.Sprintf("installed-app-restart-%d.log", restart))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal("create installed-app lifecycle log failed")
	}
	cmd := exec.CommandContext(processCtx, appExecutable)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"USERPROFILE="+home,
		"CFFIXED_USER_HOME="+home,
		"TMPDIR="+filepath.Join(home, "tmp"),
		"TEMP="+filepath.Join(home, "tmp"),
		"TMP="+filepath.Join(home, "tmp"),
		"HEXCLAW_TEST_MODE=1",
		"HEXCLAW_TEST_HOME="+home,
		"HEXCLAW_SIDECAR_PORT="+strconv.Itoa(port),
		"HEXCLAW_TEST_LLM_CONFIG_MODE=preseeded-owner-yaml",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal("start freshly built installed App failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = logFile.Close()
	})

	deadline := time.Now().Add(60 * time.Second)
	client := &http.Client{Timeout: time.Second}
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	for {
		select {
		case <-done:
			stopped = true
			_ = logFile.Close()
			t.Fatalf("freshly built installed App exited before health on restart %d", restart)
		default:
		}
		request, requestErr := http.NewRequestWithContext(processCtx, http.MethodGet, healthURL, nil)
		if requestErr != nil {
			t.Fatal("create installed-app health request failed")
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("freshly built installed App did not become healthy on restart %d", restart)
		}
		time.Sleep(100 * time.Millisecond)
	}
	sidecarPID := bug20260808OwnedSidecarPID(t, appExecutable, port)

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		stopped = true
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		stopped = true
	}
	_ = logFile.Close()
	if bug20260808PortIsOpen(processCtx, port) {
		process, findErr := os.FindProcess(sidecarPID)
		if findErr != nil {
			t.Fatalf("find owned installed-app Sidecar PID failed on restart %d", restart)
		}
		_ = process.Signal(syscall.SIGTERM)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if !bug20260808PortIsOpen(processCtx, port) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("installed-app Sidecar port remained occupied after restart %d cleanup", restart)
}

func bug20260808OwnedSidecarPID(t *testing.T, appExecutable string, port int) int {
	t.Helper()
	probeCtx, cancelProbe := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelProbe()

	output, err := exec.CommandContext(
		probeCtx,
		"lsof",
		"-nP",
		"-tiTCP:"+strconv.Itoa(port),
		"-sTCP:LISTEN",
	).Output()
	if err != nil {
		t.Fatal("resolve installed-app Sidecar listener PID failed")
	}
	pids := strings.Fields(string(output))
	if len(pids) != 1 {
		t.Fatalf("installed-app test port listener count=%d, want 1", len(pids))
	}
	pid, err := strconv.Atoi(pids[0])
	if err != nil {
		t.Fatal("parse installed-app Sidecar listener PID failed")
	}
	commandOutput, err := exec.CommandContext(
		probeCtx,
		"ps",
		"-p",
		strconv.Itoa(pid),
		"-o",
		"command=",
	).Output()
	if err != nil {
		t.Fatal("read installed-app Sidecar listener identity failed")
	}
	expected := filepath.Join(filepath.Dir(appExecutable), "hexclaw") + " serve --desktop"
	if !strings.HasPrefix(strings.TrimSpace(string(commandOutput)), expected) {
		t.Fatal("installed-app test port is owned by a foreign process")
	}
	return pid
}

func bug20260808PortIsOpen(ctx context.Context, port int) bool {
	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	connection, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func bug20260808AssertOwnerOnlyYAML(t *testing.T, configPath string) {
	t.Helper()
	fileInfo, err := os.Lstat(configPath)
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("installed-app YAML is missing, non-regular, or symbolic")
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("installed-app YAML permission=%#o, want 0600", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Lstat(filepath.Dir(configPath))
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("installed-app YAML directory is missing, non-directory, or symbolic")
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("installed-app YAML directory permission=%#o, want 0700", directoryInfo.Mode().Perm())
	}
}

func bug20260808SecurityAgentPIDs(parentCtx context.Context) map[string]struct{} {
	probeCtx, cancelProbe := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancelProbe()

	output, err := exec.CommandContext(probeCtx, "pgrep", "-x", "SecurityAgent").Output()
	if err != nil {
		return map[string]struct{}{}
	}
	set := make(map[string]struct{})
	for _, pid := range strings.Fields(string(output)) {
		set[pid] = struct{}{}
	}
	return set
}

func bug20260808StringSetDifference(after, before map[string]struct{}) []string {
	added := make([]string, 0)
	for value := range after {
		if _, existed := before[value]; !existed {
			added = append(added, value)
		}
	}
	return added
}

func bug20260808AssertSingleSecretCopy(t *testing.T, root, configPath string, secret []byte) {
	t.Helper()
	var matches []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected symbolic link in installed-app test root")
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Size() > 16<<20 {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(content, secret) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal("scan installed-app test root for duplicate Provider secret failed")
	}
	if len(matches) != 1 || matches[0] != configPath {
		t.Fatalf("Provider secret persisted outside the single owner YAML: copies=%d", len(matches))
	}
}
