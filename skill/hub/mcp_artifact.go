package hub

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var (
	npmExactVersion  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)([-+][0-9A-Za-z.-]+)?$`)
	pypiExactVersion = regexp.MustCompile(`^[0-9][0-9A-Za-z.!+_-]*$`)
)

// ValidatedMCPServer is an immutable-by-construction projection. Callers can
// obtain one only after the catalog entry has passed artifact and argv checks.
type ValidatedMCPServer struct {
	meta McpServerMeta
}

func (v ValidatedMCPServer) Name() string        { return v.meta.Name }
func (v ValidatedMCPServer) Command() string     { return v.meta.Command }
func (v ValidatedMCPServer) ConfigHint() string  { return v.meta.ConfigHint }
func (v ValidatedMCPServer) Description() string { return v.meta.Description }
func (v ValidatedMCPServer) Args() []string      { return append([]string(nil), v.meta.Args...) }
func (v ValidatedMCPServer) Artifact() MCPArtifact {
	return *v.meta.Artifact
}

// MCPServerMetaFromSkill returns a detached MCP projection for use at API and
// agentic execution boundaries.
func MCPServerMetaFromSkill(s SkillMeta) McpServerMeta {
	return skillMetaToMcp(s)
}

// ValidatePinnedMCPServer rejects unpinned/quarantined catalog entries and
// package-manager flags that could add a second, unreviewed dependency source.
func ValidatePinnedMCPServer(meta McpServerMeta) (ValidatedMCPServer, error) {
	if meta.Status == "quarantined" {
		reason := strings.TrimSpace(meta.QuarantineReason)
		if reason == "" {
			reason = "未提供隔离原因"
		}
		return ValidatedMCPServer{}, fmt.Errorf("条目 %q 已隔离: %s", meta.Name, reason)
	}
	if meta.Status != "pinned" || meta.Artifact == nil {
		return ValidatedMCPServer{}, fmt.Errorf("条目 %q 缺少 pinned artifact", meta.Name)
	}
	a := *meta.Artifact
	if strings.TrimSpace(a.Package) == "" || strings.ContainsAny(a.Package, " \t\r\n\x00") {
		return ValidatedMCPServer{}, fmt.Errorf("artifact package 非法")
	}

	exactSpec := ""
	switch a.Ecosystem {
	case "npm":
		if meta.Command != "npx" || !npmExactVersion.MatchString(a.Version) {
			return ValidatedMCPServer{}, fmt.Errorf("npm artifact 必须使用 npx 和精确 SemVer")
		}
		if a.SourceRegistry != "https://registry.npmjs.org" {
			return ValidatedMCPServer{}, fmt.Errorf("npm artifact source registry 非法")
		}
		digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(a.Integrity, "sha512-"))
		if !strings.HasPrefix(a.Integrity, "sha512-") || err != nil || len(digest) != 64 {
			return ValidatedMCPServer{}, fmt.Errorf("npm artifact 缺少有效 sha512 SRI")
		}
		exactSpec = a.Package + "@" + a.Version
		for _, arg := range meta.Args {
			if arg == "-p" || arg == "--package" || arg == "--registry" ||
				strings.HasPrefix(arg, "--package=") || strings.HasPrefix(arg, "--registry=") {
				return ValidatedMCPServer{}, fmt.Errorf("npx 条目不得注入额外 package 或 registry")
			}
		}
	case "pypi":
		if meta.Command != "uvx" || !pypiExactVersion.MatchString(a.Version) {
			return ValidatedMCPServer{}, fmt.Errorf("PyPI artifact 必须使用 uvx 和精确 PEP 440 版本")
		}
		if a.SourceRegistry != "https://pypi.org" {
			return ValidatedMCPServer{}, fmt.Errorf("PyPI artifact source registry 非法")
		}
		digest, err := hex.DecodeString(strings.TrimPrefix(a.Integrity, "sha256:"))
		if !strings.HasPrefix(a.Integrity, "sha256:") || err != nil || len(digest) != 32 {
			return ValidatedMCPServer{}, fmt.Errorf("PyPI artifact 缺少有效 sha256")
		}
		exactSpec = a.Package + "==" + a.Version
		for _, arg := range meta.Args {
			if arg == "--with" || arg == "--with-editable" || arg == "-r" || arg == "--requirements" ||
				arg == "--index-url" || arg == "--extra-index-url" || arg == "--find-links" ||
				strings.HasPrefix(arg, "--with=") || strings.HasPrefix(arg, "--requirements=") ||
				strings.HasPrefix(arg, "--index-url=") || strings.HasPrefix(arg, "--extra-index-url=") ||
				strings.HasPrefix(arg, "--find-links=") {
				return ValidatedMCPServer{}, fmt.Errorf("uvx 条目不得注入额外 dependency source")
			}
		}
	default:
		return ValidatedMCPServer{}, fmt.Errorf("不支持的 artifact ecosystem %q", a.Ecosystem)
	}

	found := false
	for _, arg := range meta.Args {
		if arg == exactSpec {
			found = true
			break
		}
	}
	if !found {
		return ValidatedMCPServer{}, fmt.Errorf("命令参数未绑定 artifact %q", exactSpec)
	}

	copyMeta := meta
	copyMeta.Args = append([]string(nil), meta.Args...)
	copyMeta.Env = cloneStringMap(meta.Env)
	copyMeta.Artifact = cloneMCPArtifact(meta.Artifact)
	return ValidatedMCPServer{meta: copyMeta}, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
