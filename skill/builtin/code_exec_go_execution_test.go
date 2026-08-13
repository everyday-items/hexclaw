package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

type codeExecGoExecutionTestSandbox struct {
	exec  func(context.Context, sandbox.Command) (*sandbox.ExecResult, error)
	close func() error
}

func (s *codeExecGoExecutionTestSandbox) Exec(ctx context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
	if s.exec == nil {
		return &sandbox.ExecResult{ExitCode: 0}, nil
	}
	return s.exec(ctx, command)
}

func (s *codeExecGoExecutionTestSandbox) Close() error {
	if s.close == nil {
		return nil
	}
	return s.close()
}

func TestCodeExecGoHelperConfigUsesTrustedBuildBoundary(t *testing.T) {
	workspace := t.TempDir()
	goBinary := filepath.Join(t.TempDir(), "go")
	writeCodeExecTestFile(t, goBinary, "fake-go")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}

	cfg, err := codeExecGoHelperConfig(
		sandbox.Config{
			Workspace:            workspace,
			MaxProcesses:         32,
			RequiredCapabilities: sandbox.UntrustedCodeIsolationCapabilities,
		},
		workspace,
		workspace,
		nil,
		goBinary,
	)
	if err != nil {
		t.Fatalf("codeExecGoHelperConfig returned error: %v", err)
	}
	if cfg.MaxProcesses != 0 {
		t.Fatalf("MaxProcesses = %d, want 0", cfg.MaxProcesses)
	}
	if cfg.ExecutionProfile != sandbox.ExecutionProfileTrustedBuild {
		t.Fatalf("ExecutionProfile = %s, want trusted-build", cfg.ExecutionProfile)
	}
	if cfg.RequiredCapabilities != sandbox.TrustedBuildIsolationCapabilities {
		t.Fatalf("RequiredCapabilities = %s, want %s", cfg.RequiredCapabilities, sandbox.TrustedBuildIsolationCapabilities)
	}
	if cfg.RequiredCapabilities.Has(sandbox.CapabilityProcessContainment) {
		t.Fatal("trusted build boundary unexpectedly requires process containment")
	}
	if !cfg.RequiredCapabilities.Has(sandbox.CapabilityProcessCreation) {
		t.Fatal("trusted build boundary does not require process creation")
	}
}

func TestCodeExecGoBuildAndFinalEnvironmentsAreSeparated(t *testing.T) {
	workspace := t.TempDir()
	goRoot := filepath.Join(t.TempDir(), "goroot")
	goBinary := filepath.Join(goRoot, "bin", "go")
	if err := os.MkdirAll(filepath.Dir(goBinary), 0700); err != nil {
		t.Fatal(err)
	}
	writeCodeExecTestFile(t, goBinary, "fake-go")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	allowed := t.TempDir()
	run := codeExecRun{
		ID:          "run-test",
		Workspace:   workspace,
		Scratch:     workspace,
		ArtifactDir: filepath.Join(workspace, "artifacts"),
		CacheDir:    filepath.Join(workspace, "cache"),
		Plan: codeExecExecutionPlan{
			GoRuntime: true,
			Toolchain: &codeExecGoToolchainDescriptor{
				Binary: goBinary,
				GOROOT: goRoot,
			},
			Environment: map[string]string{"LANG": "en_US.UTF-8"},
		},
		Config: sandbox.Config{
			Workspace:            workspace,
			ReadablePaths:        []string{allowed, goRoot, filepath.Dir(goBinary)},
			RequiredCapabilities: sandbox.TrustedBuildIsolationCapabilities,
		},
	}

	buildEnvironment, err := codeExecGoBuildEnvironment(run)
	if err != nil {
		t.Fatalf("codeExecGoBuildEnvironment returned error: %v", err)
	}
	wantBuild := map[string]string{
		"CGO_ENABLED": "0",
		"GOENV":       "off",
		"GOTOOLCHAIN": "local",
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOVCS":       "off",
	}
	for key, want := range wantBuild {
		if got := buildEnvironment[key]; got != want {
			t.Fatalf("build environment %s = %q, want %q", key, got, want)
		}
	}
	if buildEnvironment["GOROOT"] != goRoot || buildEnvironment["GOCACHE"] == "" || buildEnvironment["GOMODCACHE"] == "" {
		t.Fatalf("build environment does not bind the toolchain and private caches: %v", buildEnvironment)
	}
	if got := codeExecGoInternalBuildFlags(run); !slices.Equal(got[:4], []string{"-buildvcs=false", "-trimpath", "-p=1", "-pgo=off"}) {
		t.Fatalf("internal build flags = %v", got)
	}

	executionWorkspace := t.TempDir()
	finalConfig, err := codeExecGoFinalConfig(run, executionWorkspace)
	if err != nil {
		t.Fatalf("codeExecGoFinalConfig returned error: %v", err)
	}
	if finalConfig.RequiredCapabilities != sandbox.UntrustedCodeIsolationCapabilities {
		t.Fatalf("final capabilities = %s, want %s", finalConfig.RequiredCapabilities, sandbox.UntrustedCodeIsolationCapabilities)
	}
	if finalConfig.ExecutionProfile != sandbox.ExecutionProfileUntrusted {
		t.Fatalf("final ExecutionProfile = %s, want untrusted", finalConfig.ExecutionProfile)
	}
	if finalConfig.Workspace != resolveRealPath(executionWorkspace) {
		t.Fatalf("final workspace = %q, want %q", finalConfig.Workspace, resolveRealPath(executionWorkspace))
	}
	for _, forbidden := range []string{workspace, filepath.Join(workspace, "cache"), goRoot, filepath.Dir(goBinary)} {
		if slices.Contains(finalConfig.ReadablePaths, forbidden) {
			t.Fatalf("final readable paths expose %q: %v", forbidden, finalConfig.ReadablePaths)
		}
	}
	if !slices.ContainsFunc(finalConfig.ReadablePaths, func(path string) bool {
		return resolveRealPath(path) == resolveRealPath(allowed)
	}) {
		t.Fatalf("final readable paths lost authorized path %q: %v", allowed, finalConfig.ReadablePaths)
	}

	promotedRun := run
	promotedRun.Workspace = executionWorkspace
	promotedRun.Scratch = executionWorkspace
	promotedRun.ArtifactDir = filepath.Join(executionWorkspace, "artifacts")
	finalEnvironment, err := codeExecGoFinalEnvironment(promotedRun, executionWorkspace)
	if err != nil {
		t.Fatalf("codeExecGoFinalEnvironment returned error: %v", err)
	}
	for _, entry := range finalEnvironment {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GO") || strings.HasPrefix(key, "CGO_") ||
			strings.Contains(value, goBinary) || strings.Contains(value, goRoot) ||
			strings.Contains(value, filepath.Join(workspace, "cache", "go-build")) ||
			strings.Contains(value, filepath.Join(workspace, "cache", "gomod")) {
			t.Fatalf("final environment exposes Go build state: %q", entry)
		}
	}
}

func TestCodeExecGoCommandPolicyAcceptsOnlyRunAndTest(t *testing.T) {
	tests := []struct {
		name        string
		command     []string
		kind        codeExecGoCommandKind
		targets     []string
		programArgs []string
		testArgs    []string
		environment map[string]string
	}{
		{
			name:        "run package with program arguments",
			command:     []string{"go", "run", "./cmd/app", "--listen", "127.0.0.1"},
			kind:        codeExecGoCommandRun,
			targets:     []string{"./cmd/app"},
			programArgs: []string{"--listen", "127.0.0.1"},
		},
		{
			name:        "run Go files",
			command:     []string{"go", "run", "main.go", "helper.go", "value"},
			kind:        codeExecGoCommandRun,
			targets:     []string{"main.go", "helper.go"},
			programArgs: []string{"value"},
		},
		{
			name:        "test packages with translated flags",
			command:     []string{"env", "LANG=C", "go", "test", "./...", "-run", "TestValue", "-count=2", "-v"},
			kind:        codeExecGoCommandTest,
			targets:     []string{"./..."},
			testArgs:    []string{"-test.run=TestValue", "-test.count=2", "-test.v=true"},
			environment: map[string]string{"LANG": "C"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCodeExecGoUserCommand(tt.command)
			if err != nil {
				t.Fatalf("parseCodeExecGoUserCommand returned error: %v", err)
			}
			if got.Kind != tt.kind || !slices.Equal(got.Targets, tt.targets) ||
				!slices.Equal(got.ProgramArgs, tt.programArgs) || !slices.Equal(got.TestArgs, tt.testArgs) ||
				!maps.Equal(got.Environment, tt.environment) {
				t.Fatalf("parsed command = %#v", got)
			}
		})
	}
}

func TestCodeExecGoCommandPolicyRejectsDangerousArguments(t *testing.T) {
	tests := []struct {
		name    string
		command []string
	}{
		{name: "global working directory", command: []string{"go", "-C", ".", "test", "./..."}},
		{name: "global working directory equals", command: []string{"go", "-C=.", "test", "./..."}},
		{name: "unsupported subcommand", command: []string{"go", "generate", "./..."}},
		{name: "tool execution", command: []string{"go", "tool", "compile"}},
		{name: "run exec wrapper", command: []string{"go", "run", "-exec", "wrapper", "."}},
		{name: "toolchain wrapper", command: []string{"go", "test", "-toolexec=wrapper", "./..."}},
		{name: "compiler override", command: []string{"go", "test", "-compiler=gccgo", "./..."}},
		{name: "assembler injection", command: []string{"go", "test", "-asmflags=all=-DVALUE", "./..."}},
		{name: "compiler injection", command: []string{"go", "test", "-gcflags=all=-N", "./..."}},
		{name: "linker injection", command: []string{"go", "run", "-ldflags=-linkmode=external", "."}},
		{name: "external linker", command: []string{"go", "run", "-ldflags=-extld=/tmp/linker", "."}},
		{name: "build mode", command: []string{"go", "run", "-buildmode=plugin", "."}},
		{name: "race", command: []string{"go", "test", "-race", "./..."}},
		{name: "memory sanitizer", command: []string{"go", "test", "-msan", "./..."}},
		{name: "address sanitizer", command: []string{"go", "test", "-asan", "./..."}},
		{name: "overlay", command: []string{"go", "test", "-overlay=overlay.json", "./..."}},
		{name: "module file", command: []string{"go", "test", "-modfile=other.mod", "./..."}},
		{name: "package directory", command: []string{"go", "test", "-pkgdir=cache", "./..."}},
		{name: "profile guided build", command: []string{"go", "test", "-pgo=auto", "./..."}},
		{name: "module mode", command: []string{"go", "test", "-mod=mod", "./..."}},
		{name: "output path", command: []string{"go", "test", "-o", "result.test", "./..."}},
		{name: "compile only", command: []string{"go", "test", "-c", "./..."}},
		{name: "debug work directory", command: []string{"go", "test", "-work", "./..."}},
		{name: "JSON output", command: []string{"go", "test", "-json", "./..."}},
		{name: "fuzz", command: []string{"go", "test", "-fuzz=FuzzValue", "./..."}},
		{name: "coverage", command: []string{"go", "test", "-coverprofile=coverage.out", "./..."}},
		{name: "CPU profile", command: []string{"go", "test", "-cpuprofile=cpu.out", "./..."}},
		{name: "test internal flag", command: []string{"go", "test", "-test.run=TestValue", "./..."}},
		{name: "controlled environment", command: []string{"env", "CGO_ENABLED=1", "go", "test", "./..."}},
		{name: "absolute target", command: []string{"go", "run", "/tmp/main.go"}},
		{name: "parent target", command: []string{"go", "test", "../..."}},
		{name: "version target", command: []string{"go", "run", "example.com/tool@latest"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseCodeExecGoUserCommand(tt.command); err == nil {
				t.Fatalf("parseCodeExecGoUserCommand(%v) returned nil error", tt.command)
			}
		})
	}
}

func TestCodeExecExecutionPlanBindsParsedGoCommand(t *testing.T) {
	plan, err := bindCodeExecExecutionPlanCommand(codeExecExecutionPlan{
		GoRuntime: true,
		Toolchain: &codeExecGoToolchainDescriptor{
			Binary: filepath.Join(t.TempDir(), "go"),
			GOROOT: t.TempDir(),
		},
	}, []string{"env", "LANG=C", "go", "test", "./...", "-run", "TestValue"})
	if err != nil {
		t.Fatalf("bindCodeExecExecutionPlanCommand returned error: %v", err)
	}
	if plan.GoCommand == nil || plan.GoCommand.Kind != codeExecGoCommandTest ||
		!slices.Equal(plan.GoCommand.Targets, []string{"./..."}) ||
		!slices.Equal(plan.GoCommand.TestArgs, []string{"-test.run=TestValue"}) {
		t.Fatalf("bound Go command = %#v", plan.GoCommand)
	}
	if !plan.GoTest {
		t.Fatal("bound Go test plan did not set GoTest")
	}
}

func TestCodeExecSkillExecuteGoUsesClosedTrustedBuildArtifact(t *testing.T) {
	workspace := t.TempDir()
	testGOROOT := codeExecTestGOROOT(t)
	skill := newConfiguredTestCodeExecSkill(t, &mockSandbox{}, sandbox.Config{
		Workspace: workspace,
		Timeout:   30,
		Network:   sandbox.NetworkDisabled,
	})

	var trustedBuildCalls int
	buildClosed := false
	buildWorkspace := ""
	skill.goHelperFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		if cfg.ExecutionProfile != sandbox.ExecutionProfileTrustedBuild {
			return nil, errors.New("trusted build execution profile is required")
		}
		if cfg.RequiredCapabilities != sandbox.TrustedBuildIsolationCapabilities {
			return nil, errors.New("trusted build capabilities are required")
		}
		builtArtifact := false
		return &codeExecGoExecutionTestSandbox{
			exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
				if len(command.Args) == 0 {
					return nil, errors.New("Go helper command is empty")
				}
				switch command.Args[0] {
				case "env":
					payload, err := json.Marshal(map[string]string{
						"GOOS":        runtime.GOOS,
						"GOARCH":      runtime.GOARCH,
						"GOVERSION":   runtime.Version(),
						"GOROOT":      testGOROOT,
						"CGO_ENABLED": "0",
					})
					if err != nil {
						return nil, err
					}
					return &sandbox.ExecResult{Stdout: string(payload), ExitCode: 0}, nil
				case "tool":
					return &sandbox.ExecResult{Stdout: "compile version " + runtime.Version(), ExitCode: 0}, nil
				case "build":
					trustedBuildCalls++
					buildWorkspace = cfg.Workspace
					if cfg.MaxProcesses != 0 {
						return nil, errors.New("trusted build requested a process limit")
					}
					output := codeExecGoTestOutputPath(t, command.Args)
					writeCodeExecGoTestExecutable(t, output, "trusted-build-artifact")
					builtArtifact = true
					return &sandbox.ExecResult{ExitCode: 0}, nil
				default:
					return nil, errors.New("unexpected Go helper command")
				}
			},
			close: func() error {
				if builtArtifact {
					buildClosed = true
				}
				return nil
			},
		}, nil
	}

	var finalConfig sandbox.Config
	var finalCommand sandbox.Command
	skill.sandboxFactory = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		finalConfig = cfg
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			finalCommand = command
			if !buildClosed {
				return nil, errors.New("Go artifact execution started before the build sandbox closed")
			}
			return &sandbox.ExecResult{
				Stdout:   "TWO_PHASE_ENTRYPOINT_OK\n",
				ExitCode: 0,
				Limits: sandbox.LimitReport{
					ProcessContainment: sandbox.LimitStatusEnforced,
				},
			}, nil
		}}, nil
	}

	result, err := skill.Execute(context.Background(), map[string]any{
		"mode":     "snippet",
		"language": "go",
		"code":     "package main\nfunc main() {}\n",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if trustedBuildCalls != 1 || !buildClosed {
		t.Fatalf("trusted build lifecycle = calls:%d closed:%v", trustedBuildCalls, buildClosed)
	}
	if finalConfig.RequiredCapabilities != sandbox.UntrustedCodeIsolationCapabilities {
		t.Fatalf("final capabilities = %s, want %s", finalConfig.RequiredCapabilities, sandbox.UntrustedCodeIsolationCapabilities)
	}
	if finalCommand.Path == "" || filepath.Base(finalCommand.Path) == "go" || filepath.Base(finalCommand.Path) == "go.exe" {
		t.Fatalf("final command executed the build tool instead of the frozen artifact: %q", finalCommand.Path)
	}
	if !pathWithinResolved(finalConfig.Workspace, finalCommand.Path) ||
		pathWithinResolved(buildWorkspace, finalConfig.Workspace) || pathWithinResolved(finalConfig.Workspace, buildWorkspace) {
		t.Fatalf("final command did not use an isolated promoted workspace: cfg=%q command=%q build=%q",
			finalConfig.Workspace,
			finalCommand.Path,
			buildWorkspace,
		)
	}
	if !strings.Contains(result.Content, "TWO_PHASE_ENTRYPOINT_OK") {
		t.Fatalf("two-phase output is missing: %s", result.Content)
	}
	report, ok := result.Data.(codeExecReport)
	if !ok {
		t.Fatalf("result data = %T, want codeExecReport", result.Data)
	}
	if report.Paths["cwd"] != finalCommand.Dir {
		t.Fatalf("reported cwd = %q, want final command directory %q", report.Paths["cwd"], finalCommand.Dir)
	}
}

func TestCodeExecGoRunCompilesThenRunsArtifact(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", ".", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	buildClosed := false
	var buildCommand sandbox.Command
	var finalConfig sandbox.Config
	var finalCommand sandbox.Command
	buildFactory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		if cfg.RequiredCapabilities != sandbox.TrustedBuildIsolationCapabilities || cfg.MaxProcesses != 0 {
			t.Fatalf("build config = %#v", cfg)
		}
		return &codeExecGoExecutionTestSandbox{
			exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
				buildCommand = command
				output := codeExecGoTestOutputPath(t, command.Args)
				writeCodeExecGoTestExecutable(t, output, "compiled-run-artifact")
				return &sandbox.ExecResult{ExitCode: 0}, nil
			},
			close: func() error {
				buildClosed = true
				return nil
			},
		}, nil
	}
	runFactory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		if !buildClosed {
			t.Fatal("final sandbox was created before the build sandbox closed")
		}
		finalConfig = cfg
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			finalCommand = command
			return &sandbox.ExecResult{
				Stdout:   "run-ok\n",
				ExitCode: 0,
				Limits: sandbox.LimitReport{
					Filesystem:         sandbox.LimitStatusEnforced,
					ProcessContainment: sandbox.LimitStatusEnforced,
					Output:             sandbox.LimitStatusEnforced,
				},
			}, nil
		}}, nil
	}

	result, _, err := executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, runFactory)
	if err != nil {
		t.Fatalf("executeCodeExecGoTwoPhase returned error: %v", err)
	}
	if result == nil || result.Stdout != "run-ok\n" || result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if buildCommand.Path != run.Plan.Toolchain.Binary || len(buildCommand.Args) == 0 || buildCommand.Args[0] != "build" {
		t.Fatalf("build command = %#v", buildCommand)
	}
	buildEnvironment := make(map[string]string, len(buildCommand.Env))
	for _, entry := range buildCommand.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			buildEnvironment[key] = value
		}
	}
	for key, want := range map[string]string{
		"GOMODCACHE":  filepath.Join(run.CacheDir, "gomod"),
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOTOOLCHAIN": "local",
		"GOVCS":       "off",
	} {
		if buildEnvironment[key] != want {
			t.Fatalf("trusted build %s = %q, want %q", key, buildEnvironment[key], want)
		}
	}
	for _, flag := range []string{"-buildvcs=false", "-trimpath", "-p=1", "-pgo=off", "-o"} {
		if !slices.Contains(buildCommand.Args, flag) {
			t.Fatalf("build command missing %q: %v", flag, buildCommand.Args)
		}
	}
	if finalConfig.RequiredCapabilities != sandbox.UntrustedCodeIsolationCapabilities {
		t.Fatalf("final capabilities = %s", finalConfig.RequiredCapabilities)
	}
	if finalCommand.Path == run.Plan.Toolchain.Binary || !pathWithinResolved(finalConfig.Workspace, finalCommand.Path) ||
		pathWithinResolved(run.Workspace, finalCommand.Path) {
		t.Fatalf("final command path = %q", finalCommand.Path)
	}
	if !slices.Equal(finalCommand.Args, []string{"hello"}) {
		t.Fatalf("final command args = %v", finalCommand.Args)
	}
	assertCodeExecGoFinalCommandDoesNotExposeBuildState(t, run, finalConfig, finalCommand)
}

func TestCodeExecGoTestCompilesAllPackagesBeforeStableExecution(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	firstDir := filepath.Join(run.Workspace, "a", "shared")
	secondDir := filepath.Join(run.Workspace, "b", "shared")
	for _, directory := range []string{firstDir, secondDir} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	packages := []codeExecGoListedPackage{
		{ImportPath: "example.com/b/shared", Dir: secondDir, TestGoFiles: []string{"b_test.go"}},
		{ImportPath: "example.com/a/shared", Dir: firstDir, XTestGoFiles: []string{"a_test.go"}},
	}
	listOutput := encodeCodeExecGoListedPackages(t, packages)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "test", "./...", "-run", "TestValue", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	var compiled []string
	buildClosed := false
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{
			exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
				switch command.Args[0] {
				case "list":
					return &sandbox.ExecResult{Stdout: listOutput, StdoutBytes: int64(len(listOutput)), ExitCode: 0}, nil
				case "test":
					output := codeExecGoTestOutputPath(t, command.Args)
					compiled = append(compiled, output)
					writeCodeExecGoTestExecutable(t, output, filepath.Base(output))
					return &sandbox.ExecResult{ExitCode: 0}, nil
				default:
					t.Fatalf("unexpected build command: %v", command.Args)
					return nil, nil
				}
			},
			close: func() error {
				buildClosed = true
				return nil
			},
		}, nil
	}
	var executed []sandbox.Command
	var finalConfig sandbox.Config
	runFactory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		if !buildClosed || len(compiled) != 2 {
			t.Fatalf("execution began before all packages compiled: closed=%v compiled=%v", buildClosed, compiled)
		}
		finalConfig = cfg
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			executed = append(executed, command)
			return &sandbox.ExecResult{Stdout: "PASS\n", ExitCode: 0, Limits: sandbox.LimitReport{
				Filesystem:         sandbox.LimitStatusEnforced,
				ProcessContainment: sandbox.LimitStatusEnforced,
				Output:             sandbox.LimitStatusEnforced,
			}}, nil
		}}, nil
	}

	result, _, err := executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, runFactory)
	if err != nil {
		t.Fatalf("executeCodeExecGoTwoPhase returned error: %v", err)
	}
	if result == nil || len(executed) != 2 {
		t.Fatalf("result=%#v executed=%v", result, executed)
	}
	if filepath.Base(compiled[0]) == filepath.Base(compiled[1]) {
		t.Fatalf("same-basename packages used the same artifact name: %v", compiled)
	}
	wantFirstDir := filepath.Join(finalConfig.Workspace, "work", "a", "shared")
	wantSecondDir := filepath.Join(finalConfig.Workspace, "work", "b", "shared")
	if executed[0].Dir != resolveRealPath(wantFirstDir) || executed[1].Dir != resolveRealPath(wantSecondDir) {
		t.Fatalf("package execution order/cwd = %q, %q", executed[0].Dir, executed[1].Dir)
	}
	for _, command := range executed {
		if !slices.Equal(command.Args, []string{"-test.run=TestValue", "-test.v=true"}) {
			t.Fatalf("translated test args = %v", command.Args)
		}
	}
}

func TestCodeExecGoTestSkipsPackagesWithoutTests(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	packageDir := filepath.Join(run.Workspace, "empty")
	if err := os.MkdirAll(packageDir, 0700); err != nil {
		t.Fatal(err)
	}
	listOutput := encodeCodeExecGoListedPackages(t, []codeExecGoListedPackage{{
		ImportPath: "example.com/empty",
		Dir:        packageDir,
	}})
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "test", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	buildCommands := 0
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			buildCommands++
			if command.Args[0] != "list" {
				t.Fatalf("package without tests was compiled: %v", command.Args)
			}
			return &sandbox.ExecResult{Stdout: listOutput, StdoutBytes: int64(len(listOutput)), ExitCode: 0}, nil
		}}, nil
	}
	runCalls := 0
	result, _, err := executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		runCalls++
		return &codeExecGoExecutionTestSandbox{}, nil
	})
	if err != nil {
		t.Fatalf("executeCodeExecGoTwoPhase returned error: %v", err)
	}
	if buildCommands != 1 || runCalls != 0 || result == nil || !strings.Contains(result.Stdout, "[no test files]") {
		t.Fatalf("buildCommands=%d runCalls=%d result=%#v", buildCommands, runCalls, result)
	}
}

func TestCodeExecGoArtifactMutationDuringBuildCloseRefusesExecution(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	artifactPath := ""
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{
			exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
				artifactPath = codeExecGoTestOutputPath(t, command.Args)
				writeCodeExecGoTestExecutable(t, artifactPath, "trusted-artifact")
				return &sandbox.ExecResult{ExitCode: 0}, nil
			},
			close: func() error {
				return os.WriteFile(artifactPath, []byte("mutated-artifact"), 0700)
			},
		}, nil
	}
	runCalls := 0
	_, _, err = executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		runCalls++
		return &codeExecGoExecutionTestSandbox{}, nil
	})
	if err == nil {
		t.Fatal("mutated artifact was accepted")
	}
	if runCalls != 0 {
		t.Fatalf("final sandbox calls = %d, want 0", runCalls)
	}
}

func TestCodeExecGoArtifactReplacementDuringBuildCloseRefusesExecution(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	artifactPath := ""
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{
			exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
				artifactPath = codeExecGoTestOutputPath(t, command.Args)
				writeCodeExecGoTestExecutable(t, artifactPath, "trusted-artifact")
				return &sandbox.ExecResult{ExitCode: 0}, nil
			},
			close: func() error {
				replacement := artifactPath + ".replacement"
				if writeErr := os.WriteFile(replacement, codeExecGoTestExecutableBytes("trusted-artifact"), 0700); writeErr != nil {
					return writeErr
				}
				return os.Rename(replacement, artifactPath)
			},
		}, nil
	}
	runCalls := 0
	_, _, err = executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		runCalls++
		return &codeExecGoExecutionTestSandbox{}, nil
	})
	if err == nil {
		t.Fatal("replaced artifact was accepted")
	}
	if runCalls != 0 {
		t.Fatalf("final sandbox calls = %d, want 0", runCalls)
	}
}

func TestCodeExecGoArtifactPromotionIsolatedFromSourceMutationAfterBuildClose(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	original := codeExecGoTestExecutableBytes("trusted-artifact")
	mutated := codeExecGoTestExecutableBytes("mutated-after-close")
	var sourcePath string
	var sourceInfo os.FileInfo
	buildClosed := false
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{
			exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
				sourcePath = codeExecGoTestOutputPath(t, command.Args)
				if mkdirErr := os.MkdirAll(filepath.Dir(sourcePath), 0700); mkdirErr != nil {
					return nil, mkdirErr
				}
				if writeErr := os.WriteFile(sourcePath, original, 0700); writeErr != nil {
					return nil, writeErr
				}
				var statErr error
				sourceInfo, statErr = os.Stat(sourcePath)
				if statErr != nil {
					return nil, statErr
				}
				return &sandbox.ExecResult{ExitCode: 0}, nil
			},
			close: func() error {
				buildClosed = true
				return nil
			},
		}, nil
	}

	var finalConfig sandbox.Config
	var finalCommand sandbox.Command
	var executedBytes []byte
	var destinationInfo os.FileInfo
	runFactory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		finalConfig = cfg
		if !buildClosed {
			return nil, errors.New("final sandbox was created before the build sandbox closed")
		}
		if writeErr := os.WriteFile(sourcePath, mutated, 0700); writeErr != nil {
			return nil, writeErr
		}
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			finalCommand = command
			var readErr error
			destinationInfo, readErr = os.Stat(command.Path)
			if readErr != nil {
				return nil, readErr
			}
			executedBytes, readErr = os.ReadFile(command.Path)
			if readErr != nil {
				return nil, readErr
			}
			return &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
				Filesystem:         sandbox.LimitStatusEnforced,
				ProcessContainment: sandbox.LimitStatusEnforced,
				Output:             sandbox.LimitStatusEnforced,
			}}, nil
		}}, nil
	}

	result, _, err := executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, runFactory)
	if err != nil {
		t.Fatalf("executeCodeExecGoTwoPhase returned error: %v", err)
	}
	if result == nil || result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if !slices.Equal(executedBytes, original) {
		t.Fatalf("executed artifact bytes changed after source mutation: got %x want %x", executedBytes, original)
	}
	if finalCommand.Path == sourcePath || !pathWithinResolved(finalConfig.Workspace, finalCommand.Path) {
		t.Fatalf("final command path = %q, source = %q, workspace = %q", finalCommand.Path, sourcePath, finalConfig.Workspace)
	}
	if os.SameFile(sourceInfo, destinationInfo) {
		t.Fatal("promoted artifact reused the source inode")
	}
	if runtime.GOOS != "windows" && destinationInfo.Mode().Perm() != 0500 {
		t.Fatalf("promoted artifact permissions = %04o, want 0500", destinationInfo.Mode().Perm())
	}
	if pathWithinResolved(run.Workspace, finalConfig.Workspace) || pathWithinResolved(finalConfig.Workspace, run.Workspace) {
		t.Fatalf("final workspace %q overlaps build workspace %q", finalConfig.Workspace, run.Workspace)
	}
	for _, path := range finalConfig.ReadablePaths {
		if pathWithinResolved(run.Workspace, path) || pathWithinResolved(path, run.Workspace) ||
			pathWithinResolved(run.CacheDir, path) || pathWithinResolved(path, run.CacheDir) ||
			pathWithinResolved(run.Plan.Toolchain.GOROOT, path) || pathWithinResolved(path, run.Plan.Toolchain.GOROOT) {
			t.Fatalf("final readable path exposes build state: %q", path)
		}
	}
	for _, forbidden := range []string{run.Workspace, run.CacheDir, run.Plan.Toolchain.GOROOT, run.Plan.Toolchain.Binary} {
		if !slices.ContainsFunc(finalConfig.DeniedPaths, func(path string) bool {
			return resolveRealPath(path) == resolveRealPath(forbidden)
		}) {
			t.Fatalf("final denied paths do not contain build state %q: %v", forbidden, finalConfig.DeniedPaths)
		}
	}
	if _, statErr := os.Lstat(finalCommand.Path); !os.IsNotExist(statErr) {
		t.Fatalf("promoted artifact still exists after execution: %v", statErr)
	}
}

func TestCodeExecGoArtifactPromotionRejectsArtifactBudgetOverflow(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	run.Budget.MaxArtifactBytes = 8
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			writeCodeExecGoTestExecutable(t, codeExecGoTestOutputPath(t, command.Args), "oversized-artifact")
			return &sandbox.ExecResult{ExitCode: 0}, nil
		}}, nil
	}
	runCalls := 0
	_, _, err = executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		runCalls++
		return &codeExecGoExecutionTestSandbox{}, nil
	})
	if err == nil {
		t.Fatal("oversized Go artifact was promoted")
	}
	if runCalls != 0 {
		t.Fatalf("final sandbox calls = %d, want 0", runCalls)
	}
}

func TestCodeExecGoTwoPhaseCleansTemporaryBinariesAfterSuccess(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	var buildArtifact string
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			buildArtifact = codeExecGoTestOutputPath(t, command.Args)
			writeCodeExecGoTestExecutable(t, buildArtifact, "temporary-build-artifact")
			return &sandbox.ExecResult{ExitCode: 0}, nil
		}}, nil
	}
	var promotedArtifact string
	result, _, err := executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			promotedArtifact = command.Path
			return &sandbox.ExecResult{ExitCode: 0, Limits: sandbox.LimitReport{
				Filesystem:         sandbox.LimitStatusEnforced,
				ProcessContainment: sandbox.LimitStatusEnforced,
				Output:             sandbox.LimitStatusEnforced,
			}}, nil
		}}, nil
	})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	for _, path := range []string{buildArtifact, promotedArtifact} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("temporary binary still exists at %q: %v", path, statErr)
		}
	}
}

func TestCodeExecGoTwoPhaseCleansTemporaryBinariesAfterExecutionFailure(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	var buildArtifact string
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			buildArtifact = codeExecGoTestOutputPath(t, command.Args)
			writeCodeExecGoTestExecutable(t, buildArtifact, "temporary-build-artifact")
			return &sandbox.ExecResult{ExitCode: 0}, nil
		}}, nil
	}
	var promotedArtifact string
	execErr := errors.New("execution failed")
	_, _, err = executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			promotedArtifact = command.Path
			return nil, execErr
		}}, nil
	})
	if !errors.Is(err, execErr) {
		t.Fatalf("error = %v, want execution failure", err)
	}
	for _, path := range []string{buildArtifact, promotedArtifact} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("temporary binary still exists at %q: %v", path, statErr)
		}
	}
}

func TestCodeExecGoTwoPhaseCleansTemporaryBinariesAfterCancellation(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	var buildArtifact string
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			buildArtifact = codeExecGoTestOutputPath(t, command.Args)
			writeCodeExecGoTestExecutable(t, buildArtifact, "temporary-build-artifact")
			return &sandbox.ExecResult{ExitCode: 0}, nil
		}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var promotedArtifact string
	_, _, err = executeCodeExecGoTwoPhase(ctx, run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(ctx context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			promotedArtifact = command.Path
			cancel()
			return nil, ctx.Err()
		}}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	for _, path := range []string{buildArtifact, promotedArtifact} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("temporary binary still exists at %q: %v", path, statErr)
		}
	}
}

func TestCodeExecGoTwoPhaseReportsCleanupFailureWithoutFollowingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation is not deterministic on Windows test hosts")
	}
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			writeCodeExecGoTestExecutable(t, codeExecGoTestOutputPath(t, command.Args), "temporary-build-artifact")
			return &sandbox.ExecResult{ExitCode: 0}, nil
		}}, nil
	}
	userDirectory := t.TempDir()
	userFile := filepath.Join(userDirectory, "must-remain.txt")
	writeCodeExecTestFile(t, userFile, "user-owned")
	var promotedBin string
	_, _, err = executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
			promotedBin = filepath.Dir(command.Path)
			if removeErr := os.Remove(command.Path); removeErr != nil {
				return nil, removeErr
			}
			if removeErr := os.Remove(promotedBin); removeErr != nil {
				return nil, removeErr
			}
			if symlinkErr := os.Symlink(userDirectory, promotedBin); symlinkErr != nil {
				return nil, symlinkErr
			}
			return &sandbox.ExecResult{ExitCode: 0}, nil
		}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "promoted Go binary directory was not a regular directory") {
		t.Fatalf("error = %v, want promoted binary cleanup failure", err)
	}
	if data, readErr := os.ReadFile(userFile); readErr != nil || string(data) != "user-owned" {
		t.Fatalf("user file changed during cleanup: data=%q error=%v", data, readErr)
	}
	if _, statErr := os.Lstat(promotedBin); !os.IsNotExist(statErr) {
		t.Fatalf("invalid promoted binary entry still exists: %v", statErr)
	}
}

func TestCodeExecGoBuildCloseFailureRefusesExecution(t *testing.T) {
	run := newCodeExecGoExecutionTestRun(t)
	parsed, err := parseCodeExecGoUserCommand([]string{"go", "run", "."})
	if err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("close failed")
	buildFactory := func(sandbox.Config) (sandbox.Sandbox, error) {
		return &codeExecGoExecutionTestSandbox{
			exec: func(_ context.Context, command sandbox.Command) (*sandbox.ExecResult, error) {
				writeCodeExecGoTestExecutable(t, codeExecGoTestOutputPath(t, command.Args), "artifact")
				return &sandbox.ExecResult{ExitCode: 0}, nil
			},
			close: func() error { return closeErr },
		}, nil
	}
	runCalls := 0
	_, _, err = executeCodeExecGoTwoPhase(context.Background(), run, parsed, buildFactory, func(sandbox.Config) (sandbox.Sandbox, error) {
		runCalls++
		return &codeExecGoExecutionTestSandbox{}, nil
	})
	if !errors.Is(err, closeErr) || runCalls != 0 {
		t.Fatalf("error=%v runCalls=%d", err, runCalls)
	}
}

func newCodeExecGoExecutionTestRun(t *testing.T) codeExecRun {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "work")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	goRoot := filepath.Join(t.TempDir(), "goroot")
	goBinary := filepath.Join(goRoot, "bin", "go")
	if err := os.MkdirAll(filepath.Dir(goBinary), 0700); err != nil {
		t.Fatal(err)
	}
	writeCodeExecTestFile(t, goBinary, "fake-go-binary")
	if err := os.Chmod(goBinary, 0700); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		filepath.Join(workspace, "artifacts"),
		filepath.Join(workspace, "cache"),
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return codeExecRun{
		ID:          "run-two-phase",
		Root:        root,
		Workspace:   workspace,
		Scratch:     workspace,
		ArtifactDir: filepath.Join(workspace, "artifacts"),
		CacheDir:    filepath.Join(workspace, "cache"),
		Plan: codeExecExecutionPlan{
			GoRuntime: true,
			Toolchain: newBoundCodeExecTestToolchain(t, goBinary, goRoot, strings.Repeat("a", 64)),
		},
		Config: sandbox.Config{
			Workspace:            workspace,
			Network:              sandbox.NetworkDisabled,
			MaxOutputBytes:       64 * 1024,
			MaxStderrBytes:       64 * 1024,
			RequiredCapabilities: sandbox.UntrustedCodeIsolationCapabilities,
		},
	}
}

func codeExecGoTestOutputPath(t *testing.T, args []string) string {
	t.Helper()
	for index, argument := range args {
		if argument == "-o" && index+1 < len(args) {
			return args[index+1]
		}
	}
	t.Fatalf("command does not contain -o: %v", args)
	return ""
}

func writeCodeExecGoTestExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, codeExecGoTestExecutableBytes(content), 0700); err != nil {
		t.Fatal(err)
	}
}

func codeExecGoTestExecutableBytes(content string) []byte {
	var magic []byte
	switch runtime.GOOS {
	case "windows":
		magic = []byte{'M', 'Z', 0, 0}
	case "darwin":
		magic = []byte{0xcf, 0xfa, 0xed, 0xfe}
	default:
		magic = []byte{0x7f, 'E', 'L', 'F'}
	}
	return append(magic, []byte(content)...)
}

func encodeCodeExecGoListedPackages(t *testing.T, packages []codeExecGoListedPackage) string {
	t.Helper()
	var output strings.Builder
	for _, pkg := range packages {
		data, err := json.Marshal(pkg)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(data)
		output.WriteByte('\n')
	}
	return output.String()
}

func assertCodeExecGoFinalCommandDoesNotExposeBuildState(
	t *testing.T,
	run codeExecRun,
	cfg sandbox.Config,
	command sandbox.Command,
) {
	t.Helper()
	for _, path := range cfg.ReadablePaths {
		for _, forbidden := range []string{run.Workspace, run.CacheDir, run.Plan.Toolchain.GOROOT, filepath.Dir(run.Plan.Toolchain.Binary)} {
			if pathWithinResolved(forbidden, path) || pathWithinResolved(path, forbidden) {
				t.Fatalf("final readable path exposes build state %q: %q", forbidden, path)
			}
		}
	}
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GO") || strings.HasPrefix(key, "CGO_") ||
			strings.Contains(value, run.Plan.Toolchain.Binary) || strings.Contains(value, run.Plan.Toolchain.GOROOT) ||
			strings.Contains(value, run.Workspace) || strings.Contains(value, run.CacheDir) {
			t.Fatalf("final command environment exposes build state: %q", entry)
		}
	}
}
