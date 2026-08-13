package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/os/sandbox"
)

type codeExecGoCommandKind string

const (
	codeExecGoCommandRun  codeExecGoCommandKind = "run"
	codeExecGoCommandTest codeExecGoCommandKind = "test"

	codeExecGoListMaxOutputBytes = 4 * 1024 * 1024
	codeExecGoMaxPackages        = 64
)

type codeExecGoCommand struct {
	Kind        codeExecGoCommandKind
	Targets     []string
	ProgramArgs []string
	TestArgs    []string
	Environment map[string]string
}

type codeExecGoListedPackage struct {
	ImportPath   string   `json:"ImportPath"`
	Dir          string   `json:"Dir"`
	TestGoFiles  []string `json:"TestGoFiles"`
	XTestGoFiles []string `json:"XTestGoFiles"`
}

type codeExecGoBuiltArtifact struct {
	Path       string
	WorkingDir string
	Arguments  []string
	Identity   codeExecRegularFileIdentity
}

type codeExecGoBuildOutcome struct {
	Artifacts      []codeExecGoBuiltArtifact
	NoTestPackages []codeExecGoListedPackage
}

type codeExecGoPromotion struct {
	Run       codeExecRun
	Artifacts []codeExecGoBuiltArtifact
}

type codeExecGoResultAccumulator struct {
	result         sandbox.ExecResult
	maxStdout      int64
	maxStderr      int64
	stdoutUsed     int64
	stderrUsed     int64
	hasResult      bool
	hasFinalLimits bool
}

func newCodeExecGoResultAccumulator(run codeExecRun) *codeExecGoResultAccumulator {
	return &codeExecGoResultAccumulator{
		maxStdout: run.Config.MaxOutputBytes,
		maxStderr: run.Config.MaxStderrBytes,
	}
}

func (a *codeExecGoResultAccumulator) add(result *sandbox.ExecResult, final bool) {
	if result == nil {
		return
	}
	a.hasResult = true
	stdoutBytes := result.StdoutBytes
	if stdoutBytes == 0 && result.Stdout != "" {
		stdoutBytes = int64(len(result.Stdout))
	}
	stderrBytes := result.StderrBytes
	if stderrBytes == 0 && result.Stderr != "" {
		stderrBytes = int64(len(result.Stderr))
	}
	a.appendStdout(result.Stdout, stdoutBytes, result.StdoutTruncated)
	a.appendStderr(result.Stderr, stderrBytes, result.StderrTruncated)
	if a.result.ExitCode == 0 && result.ExitCode != 0 {
		a.result.ExitCode = result.ExitCode
	}
	if final {
		if !a.hasFinalLimits {
			a.result.Limits = result.Limits
			a.hasFinalLimits = true
		} else {
			a.result.Limits = mergeCodeExecGoLimitReports(a.result.Limits, result.Limits)
		}
	}
}

func (a *codeExecGoResultAccumulator) addGeneratedStdout(value string) {
	if value == "" {
		return
	}
	a.hasResult = true
	a.appendStdout(value, int64(len(value)), false)
}

func (a *codeExecGoResultAccumulator) appendStdout(value string, produced int64, truncated bool) {
	remaining := a.maxStdout - a.stdoutUsed
	if a.maxStdout <= 0 {
		remaining = int64(len(value))
	}
	if remaining > 0 {
		keep := min(int64(len(value)), remaining)
		a.result.Stdout += value[:keep]
	}
	a.stdoutUsed += produced
	a.result.StdoutBytes += produced
	if truncated || a.maxStdout > 0 && a.stdoutUsed > a.maxStdout {
		a.result.StdoutTruncated = true
	}
}

func (a *codeExecGoResultAccumulator) appendStderr(value string, produced int64, truncated bool) {
	remaining := a.maxStderr - a.stderrUsed
	if a.maxStderr <= 0 {
		remaining = int64(len(value))
	}
	if remaining > 0 {
		keep := min(int64(len(value)), remaining)
		a.result.Stderr += value[:keep]
	}
	a.stderrUsed += produced
	a.result.StderrBytes += produced
	if truncated || a.maxStderr > 0 && a.stderrUsed > a.maxStderr {
		a.result.StderrTruncated = true
	}
}

func (a *codeExecGoResultAccumulator) exhausted() bool {
	return a.result.StdoutTruncated || a.result.StderrTruncated ||
		a.maxStdout > 0 && a.stdoutUsed > a.maxStdout ||
		a.maxStderr > 0 && a.stderrUsed > a.maxStderr
}

func (a *codeExecGoResultAccumulator) remaining() (int64, int64) {
	stdout := a.maxStdout - a.stdoutUsed
	stderr := a.maxStderr - a.stderrUsed
	if a.maxStdout <= 0 {
		stdout = 0
	}
	if a.maxStderr <= 0 {
		stderr = 0
	}
	return stdout, stderr
}

func (a *codeExecGoResultAccumulator) value() *sandbox.ExecResult {
	if !a.hasResult {
		return nil
	}
	copy := a.result
	return &copy
}

// executeCodeExecGoTwoPhase 先让固定 Go 工具链在可信构建边界生成并冻结产物，
// 再为每个不可信产物创建严格运行边界；两个阶段之间没有宿主裸执行或回退路径。
func executeCodeExecGoTwoPhase(
	ctx context.Context,
	run codeExecRun,
	command codeExecGoCommand,
	buildFactory func(sandbox.Config) (sandbox.Sandbox, error),
	runFactory func(sandbox.Config) (sandbox.Sandbox, error),
) (result *sandbox.ExecResult, executionRun codeExecRun, returnErr error) {
	if ctx == nil {
		return nil, run, errors.New("go execution context must not be nil")
	}
	if run.Plan.Toolchain == nil {
		return nil, run, errors.New("go execution requires a bound toolchain descriptor")
	}
	if err := verifyCodeExecGoToolchainDescriptor(*run.Plan.Toolchain); err != nil {
		return nil, run, err
	}
	executionRun = run
	// 两阶段执行拥有构建与晋升二进制目录；无论成功、失败或取消都在返回前收口，
	// 且清理异常必须进入调用方可见的错误链。
	defer func() {
		if cleanupErr := cleanupCodeExecGoTemporaryBinaries(run, executionRun); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	accumulator := newCodeExecGoResultAccumulator(run)
	outcome, err := prepareCodeExecGoArtifacts(ctx, run, command, buildFactory, accumulator)
	if err != nil {
		return accumulator.value(), run, err
	}
	promotion := codeExecGoPromotion{Run: run}
	if len(outcome.Artifacts) > 0 {
		promotion, err = promoteCodeExecGoArtifacts(ctx, run, outcome.Artifacts)
		if err != nil {
			return accumulator.value(), run, fmt.Errorf("promote Go build artifacts: %w", err)
		}
	}
	for _, pkg := range outcome.NoTestPackages {
		accumulator.addGeneratedStdout(fmt.Sprintf("?\t%s\t[no test files]\n", pkg.ImportPath))
	}
	if accumulator.exhausted() && len(outcome.Artifacts) > 0 {
		return accumulator.value(), promotion.Run, errors.New("go execution output budget was exhausted during build")
	}
	for _, artifact := range promotion.Artifacts {
		stdoutRemaining, stderrRemaining := accumulator.remaining()
		if stdoutRemaining <= 0 || stderrRemaining <= 0 {
			return accumulator.value(), promotion.Run, errors.New("go execution output budget was exhausted")
		}
		result, runErr := runCodeExecGoBuiltArtifact(
			ctx,
			promotion.Run,
			artifact,
			runFactory,
			stdoutRemaining,
			stderrRemaining,
		)
		accumulator.add(result, true)
		if runErr != nil {
			return accumulator.value(), promotion.Run, runErr
		}
		if result == nil {
			return accumulator.value(), promotion.Run, errors.New("go execution returned no result")
		}
		if result.ExitCode != 0 {
			return accumulator.value(), promotion.Run, nil
		}
		if accumulator.exhausted() {
			return accumulator.value(), promotion.Run, errors.New("go execution output budget was exhausted")
		}
	}
	return accumulator.value(), promotion.Run, nil
}

// cleanupCodeExecGoTemporaryBinaries 只删除当前运行明确拥有的构建和晋升二进制目录。
func cleanupCodeExecGoTemporaryBinaries(buildRun, executionRun codeExecRun) error {
	var cleanupErr error
	if err := cleanupCodeExecGoBuildBinaryDirectory(buildRun); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := cleanupCodeExecGoPromotedBinaryDirectory(buildRun, executionRun); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func cleanupCodeExecGoBuildBinaryDirectory(run codeExecRun) error {
	workspace, err := resolveCodeExecBoundaryPath(run.Workspace)
	if err != nil {
		return fmt.Errorf("resolve Go build workspace for cleanup: %w", err)
	}
	cacheDir, err := resolveCodeExecBoundaryPath(run.CacheDir)
	if err != nil {
		return fmt.Errorf("resolve Go build cache for cleanup: %w", err)
	}
	relative, err := filepath.Rel(workspace, cacheDir)
	if err != nil {
		return fmt.Errorf("validate Go build cache ownership: %w", err)
	}
	if relative != "cache" {
		return errors.New("go build cache is not the owned workspace cache directory")
	}
	if err := removeCodeExecGoOwnedChildDirectory(cacheDir, "go-exec", "Go build binary directory"); err != nil {
		return fmt.Errorf("remove temporary Go build binaries: %w", err)
	}
	return nil
}

func cleanupCodeExecGoPromotedBinaryDirectory(buildRun, executionRun codeExecRun) error {
	if strings.TrimSpace(executionRun.Workspace) == "" {
		return nil
	}
	buildWorkspace, err := resolveCodeExecBoundaryPath(buildRun.Workspace)
	if err != nil {
		return fmt.Errorf("resolve Go build workspace for promotion cleanup: %w", err)
	}
	executionWorkspace, err := resolveCodeExecBoundaryPath(executionRun.Workspace)
	if err != nil {
		return fmt.Errorf("resolve Go execution workspace for cleanup: %w", err)
	}
	if filepath.Clean(executionWorkspace) == filepath.Clean(buildWorkspace) {
		return nil
	}
	rootPath, err := resolveCodeExecBoundaryPath(buildRun.Root)
	if err != nil {
		return fmt.Errorf("resolve Go execution root for cleanup: %w", err)
	}
	relative, err := filepath.Rel(rootPath, executionWorkspace)
	if err != nil {
		return fmt.Errorf("validate promoted Go workspace ownership: %w", err)
	}
	if filepath.IsAbs(relative) || filepath.Dir(relative) != "." ||
		!strings.HasPrefix(filepath.Base(relative), "go-runtime-") {
		return errors.New("promoted Go workspace is not owned by the current run")
	}
	if pathWithinResolved(buildWorkspace, executionWorkspace) ||
		pathWithinResolved(executionWorkspace, buildWorkspace) {
		return errors.New("promoted Go workspace overlaps the build workspace")
	}
	if err := removeCodeExecGoOwnedChildDirectory(executionWorkspace, "bin", "promoted Go binary directory"); err != nil {
		return fmt.Errorf("remove promoted Go binaries: %w", err)
	}
	return nil
}

// removeCodeExecGoOwnedChildDirectory 通过已打开父目录删除精确子项，避免跟随替换后的符号链接。
func removeCodeExecGoOwnedChildDirectory(parentPath, name, description string) (returnErr error) {
	if name == "" || name == "." || filepath.Base(name) != name {
		return fmt.Errorf("%s name is invalid", description)
	}
	parent, _, err := openCodeExecRootNoFollow(parentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s parent: %w", description, err)
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close %s parent: %w", description, closeErr))
		}
	}()

	pathInfo, err := parent.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect %s: %w", description, err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() {
		removeErr := parent.Remove(name)
		return errors.Join(
			fmt.Errorf("%s was not a regular directory", description),
			wrapCodeExecGoCleanupError(removeErr, "remove invalid "+description),
		)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return fmt.Errorf("open %s: %w", description, err)
	}
	openedInfo, statErr := child.Stat(".")
	closeErr := child.Close()
	if statErr != nil || openedInfo == nil || !os.SameFile(pathInfo, openedInfo) {
		removeErr := parent.RemoveAll(name)
		return errors.Join(
			fmt.Errorf("%s changed while opening", description),
			wrapCodeExecGoCleanupError(statErr, "inspect opened "+description),
			wrapCodeExecGoCleanupError(closeErr, "close "+description),
			wrapCodeExecGoCleanupError(removeErr, "remove changed "+description),
		)
	}
	if closeErr != nil {
		returnErr = errors.Join(returnErr, fmt.Errorf("close %s: %w", description, closeErr))
	}
	if err := parent.RemoveAll(name); err != nil {
		returnErr = errors.Join(returnErr, fmt.Errorf("remove %s: %w", description, err))
		return returnErr
	}
	if _, err := parent.Lstat(name); err == nil {
		return errors.Join(returnErr, fmt.Errorf("%s still exists after cleanup", description))
	} else if !os.IsNotExist(err) {
		return errors.Join(returnErr, fmt.Errorf("verify %s cleanup: %w", description, err))
	}
	return returnErr
}

func wrapCodeExecGoCleanupError(err error, operation string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func prepareCodeExecGoArtifacts(
	ctx context.Context,
	run codeExecRun,
	command codeExecGoCommand,
	factory func(sandbox.Config) (sandbox.Sandbox, error),
	accumulator *codeExecGoResultAccumulator,
) (outcome codeExecGoBuildOutcome, returnErr error) {
	workingDir := codeExecGoWorkingDirectory(run)
	buildEnvironment, buildErr := codeExecGoBuildEnvironment(run)
	if buildErr != nil {
		return outcome, buildErr
	}
	requested := cloneCodeExecSandboxConfig(run.Config)
	requested.MaxOutputBytes = codeExecGoListMaxOutputBytes
	requested.MaxStderrBytes = max(requested.MaxStderrBytes, codeExecGoHelperMaxStderrBytes)
	buildConfig, buildErr := codeExecGoHelperConfigWithLimit(
		requested,
		run.Workspace,
		workingDir,
		[]string{run.Plan.Toolchain.GOROOT},
		run.Plan.Toolchain.Binary,
		codeExecGoStageHardTimeout,
	)
	if buildErr != nil {
		return outcome, fmt.Errorf("create Go build boundary: %w", buildErr)
	}
	if factory == nil {
		factory = sandbox.New
	}
	buildSandbox, buildErr := factory(buildConfig)
	if buildErr != nil {
		return outcome, fmt.Errorf("create Go build sandbox: %w", buildErr)
	}
	if buildSandbox == nil {
		return outcome, errors.New("create Go build sandbox: factory returned nil sandbox")
	}
	defer func() {
		returnErr = joinCodeExecSandboxClose(ctx, returnErr, buildSandbox, "close Go build sandbox")
	}()

	execBuild := func(dir string, args []string) (*sandbox.ExecResult, error) {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		canonicalDir, boundaryErr := resolveCodeExecBoundaryPath(dir)
		if boundaryErr != nil || !pathWithinResolved(run.Workspace, canonicalDir) {
			return nil, errors.New("go build working directory is outside the workspace")
		}
		dir = canonicalDir
		environment := codeExecSortedCompleteEnvironment(dir, buildEnvironment)
		return buildSandbox.Exec(ctx, sandbox.Command{
			Path: run.Plan.Toolchain.Binary,
			Args: append([]string(nil), args...),
			Dir:  dir,
			Env:  environment,
		})
	}

	switch command.Kind {
	case codeExecGoCommandRun:
		artifactPath, artifactErr := prepareCodeExecGoArtifactOutput(run, "run", strings.Join(command.Targets, "\x00"), codeExecGoRunArtifactExtension())
		if artifactErr != nil {
			return outcome, artifactErr
		}
		args := append([]string{"build"}, codeExecGoInternalBuildFlags(run)...)
		args = append(args, "-o", artifactPath)
		args = append(args, command.Targets...)
		result, execErr := execBuild(workingDir, args)
		accumulator.add(result, false)
		if resultErr := codeExecGoBuildResultError("build Go run artifact", result, execErr); resultErr != nil {
			return outcome, resultErr
		}
		artifact, inspectErr := inspectCodeExecGoBuiltArtifact(run, artifactPath, workingDir, command.ProgramArgs)
		if inspectErr != nil {
			return outcome, fmt.Errorf("validate Go run artifact: %w", inspectErr)
		}
		outcome.Artifacts = append(outcome.Artifacts, artifact)
	case codeExecGoCommandTest:
		listArgs := append([]string{"list"}, codeExecGoInternalBuildFlags(run)...)
		listArgs = append(listArgs, "-json")
		listArgs = append(listArgs, command.Targets...)
		listResult, execErr := execBuild(workingDir, listArgs)
		if resultErr := codeExecGoBuildResultError("list Go test packages", listResult, execErr); resultErr != nil {
			accumulator.add(listResult, false)
			return outcome, resultErr
		}
		packages, listErr := decodeCodeExecGoListedPackages(run, listResult)
		if listErr != nil {
			return outcome, listErr
		}
		for _, pkg := range packages {
			if len(pkg.TestGoFiles) == 0 && len(pkg.XTestGoFiles) == 0 {
				outcome.NoTestPackages = append(outcome.NoTestPackages, pkg)
				continue
			}
			artifactPath, artifactErr := prepareCodeExecGoArtifactOutput(run, "test", pkg.ImportPath, ".test")
			if artifactErr != nil {
				return outcome, artifactErr
			}
			args := append([]string{"test"}, codeExecGoInternalBuildFlags(run)...)
			args = append(args, "-c", "-o", artifactPath, pkg.ImportPath)
			result, execErr := execBuild(workingDir, args)
			accumulator.add(result, false)
			if resultErr := codeExecGoBuildResultError("compile Go test package", result, execErr); resultErr != nil {
				return outcome, resultErr
			}
			artifact, inspectErr := inspectCodeExecGoBuiltArtifact(run, artifactPath, pkg.Dir, command.TestArgs)
			if inspectErr != nil {
				return outcome, fmt.Errorf("validate Go test artifact: %w", inspectErr)
			}
			outcome.Artifacts = append(outcome.Artifacts, artifact)
		}
	default:
		return outcome, errors.New("unsupported Go execution kind")
	}
	return outcome, nil
}

func codeExecGoBuildResultError(operation string, result *sandbox.ExecResult, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if err != nil {
		return fmt.Errorf("%s: sandbox execution failed: %w", operation, err)
	}
	if result == nil {
		return fmt.Errorf("%s: sandbox returned no result", operation)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%s: exit status %d", operation, result.ExitCode)
	}
	return nil
}

func decodeCodeExecGoListedPackages(run codeExecRun, result *sandbox.ExecResult) ([]codeExecGoListedPackage, error) {
	if result == nil || result.StdoutTruncated || result.StdoutBytes > codeExecGoListMaxOutputBytes ||
		len(result.Stdout) > codeExecGoListMaxOutputBytes {
		return nil, errors.New("go package list exceeded the output limit")
	}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	packages := make([]codeExecGoListedPackage, 0)
	seen := make(map[string]struct{})
	for {
		var pkg codeExecGoListedPackage
		if err := decoder.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode Go package list: %w", err)
		}
		if len(packages) >= codeExecGoMaxPackages {
			return nil, errors.New("go package list exceeded the package limit")
		}
		if strings.TrimSpace(pkg.ImportPath) == "" || strings.TrimSpace(pkg.Dir) == "" {
			return nil, errors.New("go package list contains an incomplete package")
		}
		if _, duplicate := seen[pkg.ImportPath]; duplicate {
			return nil, errors.New("go package list contains a duplicate import path")
		}
		seen[pkg.ImportPath] = struct{}{}
		canonicalDir, err := resolveCodeExecBoundaryPath(pkg.Dir)
		if err != nil || !pathWithinResolved(run.Workspace, canonicalDir) {
			return nil, errors.New("go package directory is outside the workspace")
		}
		pkg.Dir = canonicalDir
		packages = append(packages, pkg)
	}
	if len(packages) == 0 {
		return nil, errors.New("go package list is empty")
	}
	sort.Slice(packages, func(left, right int) bool {
		return packages[left].ImportPath < packages[right].ImportPath
	})
	return packages, nil
}

func prepareCodeExecGoArtifactOutput(run codeExecRun, kind, identity, extension string) (string, error) {
	artifactRoot := filepath.Join(run.CacheDir, "go-exec")
	if err := os.MkdirAll(artifactRoot, 0700); err != nil {
		return "", fmt.Errorf("create Go artifact directory: %w", err)
	}
	canonicalRoot, err := resolveCodeExecBoundaryPath(artifactRoot)
	if err != nil || !pathWithinResolved(run.Workspace, canonicalRoot) {
		return "", errors.New("go artifact directory is outside the workspace")
	}
	digest := sha256.Sum256([]byte(kind + "\x00" + identity))
	name := hex.EncodeToString(digest[:]) + extension
	path := filepath.Join(canonicalRoot, name)
	if _, err := os.Lstat(path); err == nil {
		return "", errors.New("go artifact output already exists")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect Go artifact output: %w", err)
	}
	return path, nil
}

func codeExecGoRunArtifactExtension() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ".bin"
}

// promoteCodeExecGoArtifacts 在 TrustedBuild 完全关闭后创建不与构建工作区重叠的私有运行区，
// 逐字节晋升所有产物；后续严格沙箱只接触该运行区中的新 inode。
func promoteCodeExecGoArtifacts(
	ctx context.Context,
	buildRun codeExecRun,
	sources []codeExecGoBuiltArtifact,
) (promotion codeExecGoPromotion, returnErr error) {
	if ctx == nil {
		return promotion, errors.New("go artifact promotion context must not be nil")
	}
	if len(sources) == 0 {
		return codeExecGoPromotion{Run: buildRun}, nil
	}
	privateWorkspace, workspaceErr := createCodeExecGoPrivateWorkspace(buildRun)
	if workspaceErr != nil {
		return promotion, workspaceErr
	}
	keepWorkspace := false
	defer func() {
		if !keepWorkspace {
			if cleanupErr := os.RemoveAll(privateWorkspace); cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("remove incomplete Go execution workspace: %w", cleanupErr))
			}
		}
	}()

	binRoot := filepath.Join(privateWorkspace, "bin")
	runtimeRoot := filepath.Join(privateWorkspace, "work")
	artifactRoot := filepath.Join(privateWorkspace, "artifacts")
	for _, directory := range []string{binRoot, runtimeRoot, artifactRoot} {
		if mkdirErr := os.Mkdir(directory, 0700); mkdirErr != nil {
			return promotion, fmt.Errorf("create Go execution workspace: %w", mkdirErr)
		}
	}

	buildRuntimeRoot, boundaryErr := resolveCodeExecBoundaryPath(codeExecGoWorkingDirectory(buildRun))
	if boundaryErr != nil || !pathWithinResolved(buildRun.Workspace, buildRuntimeRoot) {
		return promotion, errors.New("go runtime source workspace is outside the build workspace")
	}
	deniedRuntimePaths := append([]string(nil), buildRun.Config.DeniedPaths...)
	deniedRuntimePaths = append(deniedRuntimePaths, buildRun.CacheDir, buildRun.ArtifactDir, buildRun.LogDir)
	copyBudget := &codeExecStageCopyBudget{Max: buildRun.applicationBudget().MaxWorkspaceBytes}
	if copyErr := copyCodeExecStageTree(ctx, buildRuntimeRoot, runtimeRoot, deniedRuntimePaths, copyBudget); copyErr != nil {
		return promotion, fmt.Errorf("copy Go runtime workspace: %w", copyErr)
	}

	promotedRun := buildRun
	promotedRun.Workspace = privateWorkspace
	promotedRun.Scratch = privateWorkspace
	promotedRun.ArtifactDir = artifactRoot
	promotedRun.CacheDir = filepath.Join(privateWorkspace, "cache")
	promotedRun.WorkingDir = runtimeRoot
	promotedRun.GoWorkPath = ""
	promotedRun.GoVendored = false
	if strings.TrimSpace(buildRun.ProjectRoot) != "" {
		promotedRun.ProjectRoot = runtimeRoot
	}
	finalConfig, configErr := codeExecGoFinalConfig(buildRun, privateWorkspace)
	if configErr != nil {
		return promotion, configErr
	}
	promotedRun.Config = finalConfig

	remaining := buildRun.applicationBudget().MaxArtifactBytes
	promotedArtifacts := make([]codeExecGoBuiltArtifact, 0, len(sources))
	for index, source := range sources {
		workingDir, workingDirErr := codeExecGoPromotedWorkingDirectory(buildRuntimeRoot, runtimeRoot, source.WorkingDir)
		if workingDirErr != nil {
			return promotion, workingDirErr
		}
		promoted, copied, promotionErr := promoteCodeExecGoArtifact(
			ctx,
			buildRun,
			source,
			binRoot,
			workingDir,
			index,
			remaining,
		)
		if promotionErr != nil {
			return promotion, promotionErr
		}
		remaining -= copied
		promotedArtifacts = append(promotedArtifacts, promoted)
	}

	keepWorkspace = true
	return codeExecGoPromotion{Run: promotedRun, Artifacts: promotedArtifacts}, nil
}

func createCodeExecGoPrivateWorkspace(run codeExecRun) (_ string, returnErr error) {
	rootPath, rootErr := resolveCodeExecBoundaryPath(run.Root)
	if rootErr != nil {
		return "", errors.New("go execution root is invalid")
	}
	buildWorkspace, workspaceErr := resolveCodeExecBoundaryPath(run.Workspace)
	if workspaceErr != nil {
		return "", errors.New("go build workspace is invalid")
	}
	root, _, rootErr := openCodeExecRootNoFollow(rootPath)
	if rootErr != nil {
		return "", fmt.Errorf("open Go execution root: %w", rootErr)
	}
	defer joinCodeExecResourceClose(&returnErr, root, "close Go execution root")
	name := "go-runtime-" + newCodeExecRunID()
	if mkdirErr := root.Mkdir(name, 0700); mkdirErr != nil {
		return "", fmt.Errorf("create private Go execution workspace: %w", mkdirErr)
	}
	path := filepath.Join(rootPath, name)
	keep := false
	defer func() {
		if !keep {
			_ = root.RemoveAll(name)
		}
	}()
	privateWorkspace, workspaceErr := resolveCodeExecBoundaryPath(path)
	if workspaceErr != nil || privateWorkspace != filepath.Clean(path) {
		return "", errors.New("private Go execution workspace identity changed")
	}
	if pathWithinResolved(buildWorkspace, privateWorkspace) || pathWithinResolved(privateWorkspace, buildWorkspace) {
		return "", errors.New("private Go execution workspace overlaps the build workspace")
	}
	keep = true
	return privateWorkspace, nil
}

func codeExecGoPromotedWorkingDirectory(sourceRoot, destinationRoot, sourceDirectory string) (string, error) {
	canonicalSourceDirectory, boundaryErr := resolveCodeExecBoundaryPath(sourceDirectory)
	if boundaryErr != nil || !pathWithinResolved(sourceRoot, canonicalSourceDirectory) {
		return "", errors.New("go artifact working directory is outside the runtime source workspace")
	}
	sourceDirectory = canonicalSourceDirectory
	relative, relativeErr := filepath.Rel(sourceRoot, sourceDirectory)
	if relativeErr != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("go artifact working directory cannot be mapped into the execution workspace")
	}
	destination := destinationRoot
	if relative != "." {
		destination = filepath.Join(destinationRoot, relative)
	}
	if mkdirErr := os.MkdirAll(destination, 0700); mkdirErr != nil {
		return "", fmt.Errorf("create promoted Go working directory: %w", mkdirErr)
	}
	canonical, boundaryErr := resolveCodeExecBoundaryPath(destination)
	if boundaryErr != nil || !pathWithinResolved(destinationRoot, canonical) {
		return "", errors.New("promoted Go working directory escaped the execution workspace")
	}
	return canonical, nil
}

func promoteCodeExecGoArtifact(
	ctx context.Context,
	buildRun codeExecRun,
	source codeExecGoBuiltArtifact,
	destinationRoot string,
	workingDir string,
	index int,
	maxBytes int64,
) (promoted codeExecGoBuiltArtifact, copied int64, returnErr error) {
	if maxBytes <= 0 {
		return promoted, 0, errors.New("go artifact promotion size limit was exhausted")
	}
	if !filepath.IsAbs(source.Path) || !pathWithinResolved(buildRun.Workspace, source.Path) ||
		source.Identity.Info == nil || !codeExecCanonicalSHA256(source.Identity.SHA256) {
		return promoted, 0, errors.New("go build artifact identity is incomplete")
	}
	sourceRoot, _, sourceRootErr := openCodeExecRootNoFollow(filepath.Dir(source.Path))
	if sourceRootErr != nil {
		return promoted, 0, fmt.Errorf("open Go build artifact directory: %w", sourceRootErr)
	}
	defer joinCodeExecResourceClose(&returnErr, sourceRoot, "close Go build artifact directory")
	sourceName := filepath.Base(source.Path)
	pathInfo, sourcePathErr := sourceRoot.Lstat(sourceName)
	if sourcePathErr != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!sameCodeExecFileSnapshot(source.Identity.Info, pathInfo) {
		return promoted, 0, errors.New("go build artifact changed before promotion")
	}
	input, inputErr := openCodeExecRegularFileNoFollow(sourceRoot, sourceName)
	if inputErr != nil {
		return promoted, 0, fmt.Errorf("open Go build artifact: %w", inputErr)
	}
	defer joinCodeExecResourceClose(&returnErr, input, "close Go build artifact")
	openedSource, snapshotErr := snapshotCodeExecOpenedFile(input)
	if snapshotErr != nil || !openedSource.Info.Mode().IsRegular() || openedSource.Platform.Links != 1 ||
		!codeExecPathMatchesOpenedSnapshot(pathInfo, openedSource) ||
		!sameCodeExecFileSnapshot(source.Identity.Info, openedSource.Info) {
		return promoted, 0, errors.New("go build artifact changed while opening for promotion")
	}
	if openedSource.Info.Size() <= 0 || openedSource.Info.Size() > maxBytes {
		return promoted, 0, errors.New("go build artifact exceeds the promotion size limit")
	}
	var prefix [4]byte
	read, readErr := io.ReadFull(input, prefix[:])
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return promoted, 0, fmt.Errorf("inspect Go build artifact format: %w", readErr)
	}
	if !codeExecNativeExecutableMagic(prefix[:read]) {
		return promoted, 0, errors.New("go build artifact has an invalid executable format")
	}
	if _, seekErr := input.Seek(0, io.SeekStart); seekErr != nil {
		return promoted, 0, fmt.Errorf("rewind Go build artifact: %w", seekErr)
	}

	destination, _, destinationErr := openCodeExecRootNoFollow(destinationRoot)
	if destinationErr != nil {
		return promoted, 0, fmt.Errorf("open Go artifact promotion directory: %w", destinationErr)
	}
	defer joinCodeExecResourceClose(&returnErr, destination, "close Go artifact promotion directory")
	digest := sha256.Sum256([]byte(source.Identity.SHA256 + "\x00" + strconv.Itoa(index)))
	destinationName := hex.EncodeToString(digest[:]) + filepath.Ext(source.Path)
	output, outputErr := destination.OpenFile(destinationName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if outputErr != nil {
		return promoted, 0, fmt.Errorf("create promoted Go artifact: %w", outputErr)
	}
	keepDestination := false
	defer func() {
		_ = output.Close()
		if !keepDestination {
			_ = destination.Remove(destinationName)
		}
	}()

	sourceHash, copyErr := copyCodeExecGoArtifactBytes(ctx, output, input, openedSource.Info.Size())
	if copyErr != nil {
		return promoted, 0, copyErr
	}
	afterSource, afterErr := snapshotCodeExecOpenedFile(input)
	postSource, pathErr := sourceRoot.Lstat(sourceName)
	if afterErr != nil || pathErr != nil || !sameCodeExecOpenedFileSnapshot(openedSource, afterSource) ||
		!codeExecPathMatchesOpenedSnapshot(postSource, afterSource) || afterSource.Platform.Links != 1 ||
		sourceHash != source.Identity.SHA256 {
		return promoted, 0, errors.New("go build artifact changed while being promoted")
	}
	if syncErr := output.Sync(); syncErr != nil {
		return promoted, 0, fmt.Errorf("sync promoted Go artifact contents: %w", syncErr)
	}
	if chmodErr := output.Chmod(0500); chmodErr != nil {
		return promoted, 0, fmt.Errorf("set promoted Go artifact executable permission: %w", chmodErr)
	}
	if syncErr := output.Sync(); syncErr != nil {
		return promoted, 0, fmt.Errorf("sync promoted Go artifact metadata: %w", syncErr)
	}
	openedDestination, statErr := snapshotCodeExecOpenedFile(output)
	postDestination, destinationPathErr := destination.Lstat(destinationName)
	if statErr != nil || destinationPathErr != nil || !openedDestination.Info.Mode().IsRegular() ||
		openedDestination.Platform.Links != 1 || !codeExecPathMatchesOpenedSnapshot(postDestination, openedDestination) ||
		sameCodeExecOpenedFileObject(openedSource, openedDestination) ||
		openedDestination.Info.Size() != openedSource.Info.Size() {
		return promoted, 0, errors.New("promoted Go artifact did not create an independent regular file")
	}
	if closeErr := output.Close(); closeErr != nil {
		return promoted, 0, fmt.Errorf("close promoted Go artifact: %w", closeErr)
	}

	destinationPath := filepath.Join(destinationRoot, destinationName)
	destinationIdentity, inspectErr := inspectCodeExecGoExecutableArtifact(destinationPath)
	if inspectErr != nil {
		return promoted, 0, fmt.Errorf("verify promoted Go artifact: %w", inspectErr)
	}
	if destinationIdentity.SHA256 != source.Identity.SHA256 ||
		destinationIdentity.Info.Size() != openedSource.Info.Size() ||
		(runtime.GOOS != "windows" && destinationIdentity.Info.Mode().Perm() != 0500) ||
		os.SameFile(source.Identity.Info, destinationIdentity.Info) {
		return promoted, 0, errors.New("promoted Go artifact identity does not match the copied bytes")
	}
	keepDestination = true
	return codeExecGoBuiltArtifact{
		Path:       destinationPath,
		WorkingDir: workingDir,
		Arguments:  append([]string(nil), source.Arguments...),
		Identity:   destinationIdentity,
	}, openedSource.Info.Size(), nil
}

// copyCodeExecGoArtifactBytes 只复制打开时固定的字节数，并额外探测一个字节阻止增长或截断。
func copyCodeExecGoArtifactBytes(ctx context.Context, destination io.Writer, source io.Reader, expected int64) (string, error) {
	if expected <= 0 {
		return "", errors.New("go build artifact has an invalid size")
	}
	hash := sha256.New()
	writer := io.MultiWriter(destination, hash)
	buffer := make([]byte, 32*1024)
	remaining := expected
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		want := min(int64(len(buffer)), remaining)
		read, readErr := source.Read(buffer[:want])
		if read > 0 {
			written, writeErr := writer.Write(buffer[:read])
			if writeErr != nil {
				return "", fmt.Errorf("copy Go build artifact: %w", writeErr)
			}
			if written != read {
				return "", fmt.Errorf("copy Go build artifact: %w", io.ErrShortWrite)
			}
			remaining -= int64(read)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return "", errors.New("go build artifact shrank while being promoted")
			}
			return "", fmt.Errorf("copy Go build artifact: %w", readErr)
		}
		if read == 0 {
			return "", fmt.Errorf("copy Go build artifact: %w", io.ErrNoProgress)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var extra [1]byte
	read, err := source.Read(extra[:])
	if read != 0 {
		return "", errors.New("go build artifact grew while being promoted")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("verify Go build artifact boundary: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func inspectCodeExecGoBuiltArtifact(
	run codeExecRun,
	path string,
	workingDir string,
	arguments []string,
) (codeExecGoBuiltArtifact, error) {
	if !filepath.IsAbs(path) || !pathWithinResolved(run.Workspace, path) {
		return codeExecGoBuiltArtifact{}, errors.New("go artifact is outside the workspace")
	}
	canonicalDir, err := resolveCodeExecBoundaryPath(workingDir)
	if err != nil || !pathWithinResolved(run.Workspace, canonicalDir) {
		return codeExecGoBuiltArtifact{}, errors.New("go artifact working directory is outside the workspace")
	}
	identity, err := inspectCodeExecGoExecutableArtifact(path)
	if err != nil {
		return codeExecGoBuiltArtifact{}, err
	}
	return codeExecGoBuiltArtifact{
		Path:       filepath.Clean(path),
		WorkingDir: canonicalDir,
		Arguments:  append([]string(nil), arguments...),
		Identity:   identity,
	}, nil
}

func inspectCodeExecGoExecutableArtifact(path string) (_ codeExecRegularFileIdentity, returnErr error) {
	identity, err := inspectCodeExecRegularFileNoFollow(path, true)
	if err != nil {
		return codeExecRegularFileIdentity{}, err
	}
	root, _, err := openCodeExecRootNoFollow(filepath.Dir(path))
	if err != nil {
		return codeExecRegularFileIdentity{}, err
	}
	defer joinCodeExecResourceClose(&returnErr, root, "close Go executable artifact directory")
	name := filepath.Base(path)
	file, err := openCodeExecRegularFileNoFollow(root, name)
	if err != nil {
		return codeExecRegularFileIdentity{}, err
	}
	defer joinCodeExecResourceClose(&returnErr, file, "close Go executable artifact")
	opened, err := snapshotCodeExecOpenedFile(file)
	if err != nil || !sameCodeExecFileSnapshot(identity.Info, opened.Info) || opened.Platform.Links != 1 {
		return codeExecRegularFileIdentity{}, errors.New("go executable artifact identity changed while inspecting its format")
	}
	var prefix [4]byte
	read, readErr := io.ReadFull(file, prefix[:])
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return codeExecRegularFileIdentity{}, readErr
	}
	if !codeExecNativeExecutableMagic(prefix[:read]) {
		return codeExecRegularFileIdentity{}, errors.New("go executable artifact has an invalid native format")
	}
	after, afterErr := snapshotCodeExecOpenedFile(file)
	postPath, pathErr := root.Lstat(name)
	if afterErr != nil || pathErr != nil || !sameCodeExecOpenedFileSnapshot(opened, after) ||
		!codeExecPathMatchesOpenedSnapshot(postPath, after) || after.Platform.Links != 1 {
		return codeExecRegularFileIdentity{}, errors.New("go executable artifact changed while inspecting its format")
	}
	return identity, nil
}

func verifyCodeExecGoBuiltArtifact(run codeExecRun, artifact codeExecGoBuiltArtifact) error {
	if !filepath.IsAbs(artifact.Path) || !pathWithinResolved(run.Workspace, artifact.Path) || artifact.Identity.Info == nil {
		return errors.New("go artifact identity is incomplete")
	}
	current, err := inspectCodeExecGoExecutableArtifact(artifact.Path)
	if err != nil {
		return err
	}
	if current.SHA256 != artifact.Identity.SHA256 || !sameCodeExecFileSnapshot(artifact.Identity.Info, current.Info) {
		return errors.New("go artifact identity changed")
	}
	return nil
}

func runCodeExecGoBuiltArtifact(
	ctx context.Context,
	run codeExecRun,
	artifact codeExecGoBuiltArtifact,
	factory func(sandbox.Config) (sandbox.Sandbox, error),
	maxStdout int64,
	maxStderr int64,
) (result *sandbox.ExecResult, returnErr error) {
	if err := verifyCodeExecGoBuiltArtifact(run, artifact); err != nil {
		return nil, fmt.Errorf("verify Go artifact before execution: %w", err)
	}
	cfg := cloneCodeExecSandboxConfig(run.Config)
	if cfg.Workspace != run.Workspace || !pathWithinResolved(run.Workspace, artifact.Path) {
		return nil, errors.New("promoted Go artifact is outside the final execution workspace")
	}
	cfg = withCodeExecRequiredCapabilities(cfg)
	cfg.MaxOutputBytes = maxStdout
	cfg.MaxStderrBytes = maxStderr
	if factory == nil {
		factory = sandbox.New
	}
	sb, factoryErr := factory(cfg)
	if factoryErr != nil {
		return nil, fmt.Errorf("create Go execution sandbox: %w", factoryErr)
	}
	if sb == nil {
		return nil, errors.New("create Go execution sandbox: factory returned nil sandbox")
	}
	defer func() {
		returnErr = joinCodeExecSandboxClose(ctx, returnErr, sb, "close Go execution sandbox")
	}()
	if verifyErr := verifyCodeExecGoBuiltArtifact(run, artifact); verifyErr != nil {
		return nil, fmt.Errorf("verify Go artifact after sandbox creation: %w", verifyErr)
	}
	environment, environmentErr := codeExecGoFinalEnvironment(run, artifact.WorkingDir)
	if environmentErr != nil {
		return nil, environmentErr
	}
	return sb.Exec(ctx, sandbox.Command{
		Path: artifact.Path,
		Args: append([]string(nil), artifact.Arguments...),
		Dir:  artifact.WorkingDir,
		Env:  environment,
	})
}

func mergeCodeExecGoLimitReports(left, right sandbox.LimitReport) sandbox.LimitReport {
	return sandbox.LimitReport{
		Network:            mergeCodeExecGoLimitStatus(left.Network, right.Network),
		Memory:             mergeCodeExecGoLimitStatus(left.Memory, right.Memory),
		Processes:          mergeCodeExecGoLimitStatus(left.Processes, right.Processes),
		ProcessContainment: mergeCodeExecGoLimitStatus(left.ProcessContainment, right.ProcessContainment),
		Storage:            mergeCodeExecGoLimitStatus(left.Storage, right.Storage),
		Output:             mergeCodeExecGoLimitStatus(left.Output, right.Output),
		Filesystem:         mergeCodeExecGoLimitStatus(left.Filesystem, right.Filesystem),
	}
}

func mergeCodeExecGoLimitStatus(left, right sandbox.LimitStatus) sandbox.LimitStatus {
	if left == right {
		return left
	}
	if left == sandbox.LimitStatusEnforced && right == sandbox.LimitStatusEnforced {
		return sandbox.LimitStatusEnforced
	}
	return sandbox.LimitStatusUnsupported
}

func codeExecGoBuildEnvironment(run codeExecRun) (map[string]string, error) {
	if run.Plan.Toolchain == nil {
		return nil, errors.New("go build requires a bound toolchain descriptor")
	}
	home := filepath.Join(run.CacheDir, "go-home")
	tempDir := filepath.Join(run.CacheDir, "go-tmp")
	goCache := filepath.Join(run.CacheDir, "go-build")
	moduleCache := filepath.Join(run.CacheDir, "gomod")
	for _, directory := range []string{home, tempDir, goCache, moduleCache} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, fmt.Errorf("create Go build directory: %w", err)
		}
	}
	environment := make(map[string]string, len(run.Plan.Environment)+20)
	for key, value := range run.Plan.Environment {
		if codeExecControlledEnvironmentKey(key) {
			return nil, fmt.Errorf("go execution cannot override controlled environment key %s", key)
		}
		environment[key] = value
	}
	goWork := "off"
	if strings.TrimSpace(run.GoWorkPath) != "" {
		goWork = run.GoWorkPath
	}
	environment["APPDATA"] = filepath.Join(home, "AppData", "Roaming")
	environment["CGO_ENABLED"] = "0"
	environment["GOCACHE"] = goCache
	environment["GOENV"] = "off"
	environment["GOFLAGS"] = ""
	environment["GOMODCACHE"] = moduleCache
	environment["GONOPROXY"] = ""
	environment["GONOSUMDB"] = ""
	environment["GOPROXY"] = "off"
	environment["GOROOT"] = run.Plan.Toolchain.GOROOT
	environment["GOSUMDB"] = "off"
	environment["GOTOOLCHAIN"] = "local"
	environment["GOVCS"] = "off"
	environment["GOWORK"] = goWork
	environment["HOME"] = home
	environment["LOCALAPPDATA"] = filepath.Join(home, "AppData", "Local")
	environment["PATH"] = strings.Join(compactCleanPaths([]string{
		filepath.Dir(run.Plan.Toolchain.Binary),
		"/usr/bin",
		"/bin",
	}), string(os.PathListSeparator))
	environment["TMP"] = tempDir
	environment["TEMP"] = tempDir
	environment["TMPDIR"] = tempDir
	environment["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	if err := ensureCodeExecGoTelemetryOff(environment); err != nil {
		return nil, err
	}
	return environment, nil
}

func codeExecGoInternalBuildFlags(run codeExecRun) []string {
	flags := []string{"-buildvcs=false", "-trimpath", "-p=1", "-pgo=off"}
	switch {
	case run.GoVendored:
		flags = append(flags, "-mod=vendor")
	case fileExists(filepath.Join(codeExecGoWorkingDirectory(run), "go.mod")):
		flags = append(flags, "-mod=readonly")
	}
	return flags
}

func codeExecGoWorkingDirectory(run codeExecRun) string {
	if strings.TrimSpace(run.ProjectRoot) != "" {
		return run.ProjectRoot
	}
	return run.Workspace
}

func codeExecGoFinalConfig(buildRun codeExecRun, executionWorkspace string) (sandbox.Config, error) {
	cfg := cloneCodeExecSandboxConfig(buildRun.Config)
	toolchain := buildRun.Plan.Toolchain
	if toolchain == nil {
		return sandbox.Config{}, errors.New("go execution requires a bound toolchain descriptor")
	}
	executionWorkspace, err := resolveCodeExecBoundaryPath(executionWorkspace)
	if err != nil {
		return sandbox.Config{}, errors.New("go execution workspace is invalid")
	}
	buildWorkspace, err := resolveCodeExecBoundaryPath(buildRun.Workspace)
	if err != nil || pathWithinResolved(buildWorkspace, executionWorkspace) ||
		pathWithinResolved(executionWorkspace, buildWorkspace) {
		return sandbox.Config{}, errors.New("go execution workspace overlaps the build workspace")
	}
	forbiddenBuildPaths := []string{
		buildWorkspace,
		buildRun.CacheDir,
		toolchain.GOROOT,
		toolchain.Binary,
	}
	readable := make([]string, 0, len(cfg.ReadablePaths))
	for _, path := range cfg.ReadablePaths {
		overlapsBuildState := false
		for _, forbidden := range forbiddenBuildPaths {
			if strings.TrimSpace(forbidden) != "" &&
				(pathWithinResolved(forbidden, path) || pathWithinResolved(path, forbidden)) {
				overlapsBuildState = true
				break
			}
		}
		if !overlapsBuildState {
			readable = append(readable, path)
		}
	}
	cfg.ReadablePaths = compactCleanPaths(readable)
	denied, err := canonicalCodeExecPaths(append(cfg.DeniedPaths, forbiddenBuildPaths...))
	if err != nil {
		return sandbox.Config{}, fmt.Errorf("derive Go execution denied paths: %w", err)
	}
	cfg.DeniedPaths = denied
	cfg.Workspace = executionWorkspace
	cfg.Network = sandbox.NetworkDisabled
	cfg.RequiredCapabilities = 0
	return withCodeExecRequiredCapabilities(cfg), nil
}

func codeExecGoFinalEnvironment(run codeExecRun, workingDir string) ([]string, error) {
	workingDir, err := resolveCodeExecBoundaryPath(workingDir)
	if err != nil || !pathWithinResolved(run.Workspace, workingDir) {
		return nil, errors.New("go execution working directory is outside the workspace")
	}
	home := run.Scratch
	tempDir := filepath.Join(run.Scratch, "tmp")
	for _, directory := range []string{
		home,
		tempDir,
		filepath.Join(home, ".config"),
		filepath.Join(home, "AppData", "Roaming"),
		filepath.Join(home, "AppData", "Local"),
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, fmt.Errorf("create Go execution directory: %w", err)
		}
	}
	environment := map[string]string{
		"APPDATA":              filepath.Join(home, "AppData", "Roaming"),
		"HEXCLAW_ARTIFACT_DIR": run.ArtifactDir,
		"HEXCLAW_RUN_ID":       run.ID,
		"HEXCLAW_WORKSPACE":    run.Scratch,
		"HOME":                 home,
		"LANG":                 "C.UTF-8",
		"LC_ALL":               "C.UTF-8",
		"LOCALAPPDATA":         filepath.Join(home, "AppData", "Local"),
		"LOGNAME":              "hexclaw",
		"PATH":                 codeExecGoFinalRuntimePath(),
		"PWD":                  workingDir,
		"TEMP":                 tempDir,
		"TMP":                  tempDir,
		"TMPDIR":               tempDir,
		"USER":                 "hexclaw",
		"XDG_CONFIG_HOME":      filepath.Join(home, ".config"),
	}
	for key, value := range run.Plan.Environment {
		if codeExecControlledEnvironmentKey(key) {
			return nil, fmt.Errorf("go execution cannot override controlled environment key %s", key)
		}
		environment[key] = value
	}
	if runtime.GOOS == "windows" {
		if systemRoot := strings.TrimSpace(os.Getenv("SystemRoot")); systemRoot != "" {
			environment["SystemRoot"] = systemRoot
		}
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	complete := make([]string, 0, len(keys))
	for _, key := range keys {
		complete = append(complete, key+"="+environment[key])
	}
	return complete, nil
}

func codeExecGoFinalRuntimePath() string {
	if runtime.GOOS == "windows" {
		if systemRoot := strings.TrimSpace(os.Getenv("SystemRoot")); systemRoot != "" {
			return filepath.Join(systemRoot, "System32")
		}
		return ""
	}
	return strings.Join([]string{"/usr/bin", "/bin"}, string(os.PathListSeparator))
}

// parseCodeExecGoUserCommand 将用户 argv 收敛为 run/test 两种明确意图。
// 构建参数全部由系统生成，用户输入不得进入编译器、链接器或工具执行控制面。
func parseCodeExecGoUserCommand(command []string) (codeExecGoCommand, error) {
	goIndex, err := codeExecDirectGoCommandIndex(command)
	if err != nil {
		return codeExecGoCommand{}, err
	}
	environment := make(map[string]string, goIndex)
	for index := 1; index < goIndex; index++ {
		key, value, _ := strings.Cut(command[index], "=")
		environment[key] = value
	}
	args := command[goIndex+1:]
	if len(args) == 0 {
		return codeExecGoCommand{}, errors.New("go execution requires run or test")
	}
	if args[0] != strings.TrimSpace(args[0]) || strings.HasPrefix(args[0], "-") {
		return codeExecGoCommand{}, errors.New("go execution does not accept global flags")
	}
	switch strings.ToLower(args[0]) {
	case string(codeExecGoCommandRun):
		parsed, err := parseCodeExecGoRunCommand(args[1:])
		parsed.Environment = environment
		return parsed, err
	case string(codeExecGoCommandTest):
		parsed, err := parseCodeExecGoTestCommand(args[1:])
		parsed.Environment = environment
		return parsed, err
	default:
		return codeExecGoCommand{}, errors.New("go execution accepts only run or test")
	}
}

func parseCodeExecGoRunCommand(args []string) (codeExecGoCommand, error) {
	if len(args) == 0 {
		return codeExecGoCommand{}, errors.New("go run requires a local package or Go file target")
	}
	if strings.HasPrefix(args[0], "-") {
		return codeExecGoCommand{}, fmt.Errorf("go run flag %q is not allowed", codeExecGoFlagName(args[0]))
	}
	parsed := codeExecGoCommand{Kind: codeExecGoCommandRun}
	if codeExecGoFileTarget(args[0]) {
		for len(parsed.Targets) < len(args) && codeExecGoFileTarget(args[len(parsed.Targets)]) {
			target := args[len(parsed.Targets)]
			if err := validateCodeExecGoLocalTarget(target, true); err != nil {
				return codeExecGoCommand{}, err
			}
			parsed.Targets = append(parsed.Targets, target)
		}
	} else {
		if err := validateCodeExecGoLocalTarget(args[0], false); err != nil {
			return codeExecGoCommand{}, err
		}
		parsed.Targets = append(parsed.Targets, args[0])
	}
	for _, argument := range args[len(parsed.Targets):] {
		if strings.IndexByte(argument, 0) >= 0 {
			return codeExecGoCommand{}, errors.New("go run program argument contains NUL")
		}
		parsed.ProgramArgs = append(parsed.ProgramArgs, argument)
	}
	return parsed, nil
}

func parseCodeExecGoTestCommand(args []string) (codeExecGoCommand, error) {
	parsed := codeExecGoCommand{Kind: codeExecGoCommandTest}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			if err := validateCodeExecGoLocalTarget(argument, false); err != nil {
				return codeExecGoCommand{}, err
			}
			parsed.Targets = append(parsed.Targets, argument)
			continue
		}

		name, value, hasValue := strings.Cut(argument, "=")
		canonicalName := strings.TrimLeft(name, "-")
		if canonicalName == "" || strings.HasPrefix(strings.ToLower(canonicalName), "test.") {
			return codeExecGoCommand{}, fmt.Errorf("go test flag %q is not allowed", name)
		}
		if codeExecGoTestBooleanFlag(canonicalName) {
			if !hasValue {
				value = "true"
			} else if _, err := strconv.ParseBool(value); err != nil {
				return codeExecGoCommand{}, fmt.Errorf("go test flag %q requires a boolean value", name)
			}
			parsed.TestArgs = append(parsed.TestArgs, "-test."+canonicalName+"="+value)
			continue
		}
		if !codeExecGoTestValueFlag(canonicalName) {
			return codeExecGoCommand{}, fmt.Errorf("go test flag %q is not allowed", name)
		}
		if !hasValue {
			index++
			if index >= len(args) {
				return codeExecGoCommand{}, fmt.Errorf("go test flag %q requires a value", name)
			}
			value = args[index]
		}
		if err := validateCodeExecGoTestFlagValue(canonicalName, value); err != nil {
			return codeExecGoCommand{}, err
		}
		parsed.TestArgs = append(parsed.TestArgs, "-test."+canonicalName+"="+value)
	}
	if len(parsed.Targets) == 0 {
		parsed.Targets = []string{"."}
	}
	return parsed, nil
}

func codeExecGoTestBooleanFlag(name string) bool {
	switch name {
	case "benchmem", "failfast", "short", "v":
		return true
	default:
		return false
	}
}

func codeExecGoTestValueFlag(name string) bool {
	switch name {
	case "bench", "benchtime", "count", "cpu", "list", "parallel", "run", "shuffle", "skip", "timeout":
		return true
	default:
		return false
	}
}

func validateCodeExecGoTestFlagValue(name, value string) error {
	if value == "" || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("go test flag %q requires a non-empty value", "-"+name)
	}
	switch name {
	case "count", "parallel":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 || parsed > 256 {
			return fmt.Errorf("go test flag %q is outside the allowed range", "-"+name)
		}
	case "cpu":
		for _, item := range strings.Split(value, ",") {
			parsed, err := strconv.Atoi(item)
			if err != nil || parsed <= 0 || parsed > 256 {
				return errors.New("go test flag -cpu is outside the allowed range")
			}
		}
	case "timeout":
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return errors.New("go test flag -timeout requires a positive duration")
		}
	case "benchtime":
		if strings.HasSuffix(value, "x") {
			count, err := strconv.Atoi(strings.TrimSuffix(value, "x"))
			if err != nil || count <= 0 {
				return errors.New("go test flag -benchtime is invalid")
			}
		} else if duration, err := time.ParseDuration(value); err != nil || duration <= 0 {
			return errors.New("go test flag -benchtime is invalid")
		}
	}
	return nil
}

func codeExecGoFlagName(argument string) string {
	name, _, _ := strings.Cut(argument, "=")
	return name
}

func codeExecGoFileTarget(target string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(target)), ".go")
}

func validateCodeExecGoLocalTarget(target string, goFile bool) error {
	if target == "" || target != strings.TrimSpace(target) || strings.IndexByte(target, 0) >= 0 ||
		filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.Contains(target, `\`) ||
		strings.Contains(target, "@") {
		return fmt.Errorf("go target %q must be a local workspace path", target)
	}
	clean := filepath.Clean(filepath.FromSlash(target))
	if !filepath.IsLocal(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("go target %q must remain inside the workspace", target)
	}
	if goFile {
		if !codeExecGoFileTarget(target) || strings.HasSuffix(strings.ToLower(target), "_test.go") {
			return fmt.Errorf("go run target %q must be a non-test Go file", target)
		}
		return nil
	}
	if target != "." && !strings.HasPrefix(target, "./") {
		return fmt.Errorf("go target %q must use an explicit local package path", target)
	}
	return nil
}
