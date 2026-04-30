package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func TestValidateManifest_RejectsEmptyName(t *testing.T) {
	err := ValidateManifest(Manifest{Version: "1.0", MinHostVersion: "0.4.0"}, "0.4.0")
	if err == nil {
		t.Error("空 Name 应报错")
	}
	var me *ManifestError
	if !errors.As(err, &me) {
		t.Errorf("应返回 *ManifestError；got %T", err)
	}
}

func TestValidateManifest_RejectsPathTraversal(t *testing.T) {
	cases := []string{"../etc", "a/b", "a\\b", "../foo"}
	for _, name := range cases {
		err := ValidateManifest(Manifest{Name: name, Version: "1.0", MinHostVersion: "0.4.0"}, "0.4.0")
		if err == nil {
			t.Errorf("%q 应被拒；got nil", name)
		}
	}
}

func TestValidateManifest_RequiresVersion(t *testing.T) {
	err := ValidateManifest(Manifest{Name: "x", MinHostVersion: "0.4.0"}, "0.4.0")
	if err == nil {
		t.Error("空 Version 应报错")
	}
}

func TestValidateManifest_HostVersionTooLow(t *testing.T) {
	err := ValidateManifest(Manifest{Name: "x", Version: "1.0", MinHostVersion: "0.5.0"}, "0.4.0")
	if err == nil {
		t.Error("MinHostVersion=0.5.0 > host 0.4.0 应被拒")
	}
}

func TestValidateManifest_HostVersionEqual(t *testing.T) {
	err := ValidateManifest(Manifest{Name: "x", Version: "1.0", MinHostVersion: "0.4.0"}, "0.4.0")
	if err != nil {
		t.Errorf("等于 host version 应通过；got %v", err)
	}
}

func TestValidateManifest_HostVersionLower(t *testing.T) {
	err := ValidateManifest(Manifest{Name: "x", Version: "1.0", MinHostVersion: "0.3.0"}, "0.4.0")
	if err != nil {
		t.Errorf("MinHost=0.3 < host=0.4 应通过；got %v", err)
	}
}

func TestValidateManifest_UnknownCapability(t *testing.T) {
	err := ValidateManifest(Manifest{
		Name:           "x",
		Version:        "1.0",
		MinHostVersion: "0.4.0",
		Capabilities:   []Capability{"super.power"},
	}, "0.4.0")
	if err == nil {
		t.Error("未知 capability 应被拒")
	}
}

func TestValidateManifest_KnownCapabilities(t *testing.T) {
	err := ValidateManifest(Manifest{
		Name:           "x",
		Version:        "1.0",
		MinHostVersion: "0.4.0",
		Capabilities:   []Capability{CapReadSkills, CapEmitEvents, CapNetwork, CapFileRead, CapFileWrite},
	}, "0.4.0")
	if err != nil {
		t.Errorf("已知 caps 应通过；got %v", err)
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.4.0", 0},
		{"0.4.0", "0.4.1", -1},
		{"0.5.0", "0.4.10", 1},
		{"0.4.0-beta", "0.4.0", 0}, // 后缀被截断
		{"1.0.0", "0.9.99", 1},
	}
	for _, c := range cases {
		got := compareVersion(c.a, c.b)
		if got != c.want {
			t.Errorf("compareVersion(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

type fakeSkillReader struct{ names []string }

func (f *fakeSkillReader) SkillNames() []string { return f.names }

func TestBuildExtensionContext_FlagOff(t *testing.T) {
	m := Manifest{Capabilities: []Capability{CapReadSkills, CapEmitEvents}}
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagPluginExtensionV1: false,
	})
	c := BuildExtensionContext(m, "0.4.0", flags, &fakeSkillReader{}, nil, nil, nil, nil)
	if c.SkillsReader != nil || c.EmitEvent != nil {
		t.Error("flag OFF 时所有 capability 字段应为 nil")
	}
	if c.HostVersion != "0.4.0" {
		t.Error("HostVersion 总应填充")
	}
}

func TestBuildExtensionContext_OnlyDeclaredCapsExposed(t *testing.T) {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagPluginExtensionV1: true,
	})
	emit := func(string, map[string]any) {}
	httpC := struct{}{}
	fsRead := func(string) ([]byte, error) { return nil, nil }
	fsWrite := func(string, []byte) error { return nil }

	// 只申请 ReadSkills + Network
	m := Manifest{Capabilities: []Capability{CapReadSkills, CapNetwork}}
	c := BuildExtensionContext(m, "0.4.0", flags, &fakeSkillReader{}, emit, httpC, fsRead, fsWrite)

	if c.SkillsReader == nil {
		t.Error("申请 ReadSkills 应被注入")
	}
	if c.HTTPClient == nil {
		t.Error("申请 Network 应被注入")
	}
	if c.EmitEvent != nil {
		t.Error("未申请 EmitEvents 不应被注入")
	}
	if c.FSRead != nil {
		t.Error("未申请 FileRead 不应被注入")
	}
	if c.FSWritePending != nil {
		t.Error("未申请 FileWrite 不应被注入")
	}
}

func TestBuildExtensionContext_NilFlagsReturnsEmpty(t *testing.T) {
	m := Manifest{Capabilities: []Capability{CapReadSkills}}
	c := BuildExtensionContext(m, "0.4.0", nil, &fakeSkillReader{}, nil, nil, nil, nil)
	if c.SkillsReader != nil {
		t.Error("nil flags 应等价于 OFF")
	}
}

func TestSortedCapabilities(t *testing.T) {
	m := Manifest{Capabilities: []Capability{CapNetwork, CapReadSkills, CapFileWrite}}
	got := SortedCapabilities(m)
	want := []Capability{CapFileWrite, CapNetwork, CapReadSkills}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: %v vs %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("sort 错；got %v want %v", got, want)
		}
	}
}

func TestManifestError_Format(t *testing.T) {
	err := &ManifestError{PluginName: "x", Reason: "boom"}
	if !strings.Contains(err.Error(), "x") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("ManifestError.Error 应含 name 和 reason; got %s", err.Error())
	}
}
