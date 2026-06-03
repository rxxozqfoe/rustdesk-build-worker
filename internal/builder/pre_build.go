package builder

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PreBuilder handles compiling RustDesk from source.
type PreBuilder struct {
	srcDir      string
	worktreeDir string
	logDir      string

	mu        sync.Mutex
	cancelCmd *exec.Cmd
}

func NewPreBuilder(srcDir, worktreeDir, logDir string) *PreBuilder {
	return &PreBuilder{
		srcDir:      srcDir,
		worktreeDir: worktreeDir,
		logDir:      logDir,
	}
}

// BuildResult contains the output of a successful build.
type BuildResult struct {
	OutputDir string // path to the build output folder
	LogPath   string // path to the build log
}

var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+`)

// ListVersions returns available git tags from the source tree.
func (b *PreBuilder) ListVersions() ([]string, error) {
	srcDir, _ := filepath.Abs(b.srcDir)

	cmd := exec.Command("git", "-C", srcDir, "tag", "--list", "--sort=-version:refname")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git tag failed: %w", err)
	}

	var versions []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		tag := strings.TrimSpace(line)
		if tag != "" && semverRe.MatchString(tag) {
			versions = append(versions, tag)
		}
	}
	return versions, nil
}

// Build executes the full build pipeline and returns the output directory.
func (b *PreBuilder) Build(version, platform, arch, pubKey string) (*BuildResult, error) {
	if platform != "linux" {
		return nil, fmt.Errorf("only linux platform is supported for builds")
	}
	if arch != "x86_64" && arch != "aarch64" {
		return nil, fmt.Errorf("unsupported architecture: %s", arch)
	}

	worktreeDir, _ := filepath.Abs(b.worktreeDir)
	srcDir, _ := filepath.Abs(b.srcDir)

	if err := os.MkdirAll(b.logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log dir: %w", err)
	}
	logPath := filepath.Join(b.logDir, fmt.Sprintf("prebuild_%s_%s_%s_%d.log", version, platform, arch, time.Now().Unix()))

	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %v", err)
	}
	defer func() { _ = logFile.Close() }()

	logger := bufio.NewWriter(logFile)
	writeLog := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		_, _ = fmt.Fprintf(logger, "[%s] %s\n", time.Now().Format("15:04:05"), msg)
		_ = logger.Flush()
	}

	writeLog("Pre-build started: version=%s platform=%s arch=%s", version, platform, arch)

	if err := b.ensureWorktree(srcDir, worktreeDir, writeLog); err != nil {
		return nil, fmt.Errorf("worktree setup failed: %v", err)
	}

	writeLog("Fetching tags and checking out version %s...", version)
	if err := b.runInDir(worktreeDir, logFile, "git", "fetch", "origin", "--tags"); err != nil {
		return nil, fmt.Errorf("git fetch failed: %v", err)
	}
	if err := b.runInDir(worktreeDir, logFile, "git", "checkout", "--force", version); err != nil {
		return nil, fmt.Errorf("git checkout %s failed: %v", version, err)
	}
	_ = b.runInDir(worktreeDir, logFile, "git", "clean", "-fd") // best-effort cleanup

	writeLog("Initializing git submodules...")
	if err := b.runInDir(worktreeDir, logFile, "git", "submodule", "update", "--init", "--recursive"); err != nil {
		return nil, fmt.Errorf("git submodule update failed: %v", err)
	}

	if pubKey != "" {
		writeLog("Patching signing public key in common.rs...")
		commonRsPath := filepath.Join(worktreeDir, "src", "common.rs")
		if err := patchPublicKey(commonRsPath, pubKey); err != nil {
			return nil, fmt.Errorf("failed to patch public key: %v", err)
		}
	}

	vcpkgRoot := os.Getenv("VCPKG_ROOT")
	if vcpkgRoot == "" {
		return nil, fmt.Errorf("VCPKG_ROOT environment variable is not set")
	}
	writeLog("VCPKG_ROOT=%s", vcpkgRoot)

	if _, err := exec.LookPath("cargo"); err != nil {
		return nil, fmt.Errorf("cargo not found in PATH")
	}
	if _, err := exec.LookPath("flutter"); err != nil {
		return nil, fmt.Errorf("flutter not found in PATH")
	}

	// Step 1: flutter_rust_bridge codegen
	writeLog("Step 1/4: Generating flutter_rust_bridge code...")
	pubspecPath := filepath.Join(worktreeDir, "flutter", "pubspec.yaml")
	_ = b.runBuildCmd(worktreeDir, logFile, "sed", "-i", // best-effort patch
		"s/extended_text: 14.0.0/extended_text: 13.0.0/g", pubspecPath)
	if err := b.runBuildCmd(filepath.Join(worktreeDir, "flutter"), logFile, "flutter", "pub", "get"); err != nil {
		return nil, fmt.Errorf("flutter pub get failed: %v", err)
	}
	if err := b.runBuildCmd(worktreeDir, logFile,
		"flutter_rust_bridge_codegen",
		"--rust-input", "./src/flutter_ffi.rs",
		"--dart-output", "./flutter/lib/generated_bridge.dart",
		"--c-output", "./flutter/macos/Runner/bridge_generated.h",
	); err != nil {
		return nil, fmt.Errorf("flutter_rust_bridge_codegen failed: %v", err)
	}

	// Step 2: Compile Rust library
	features := "flutter"
	writeLog("Step 2/4: Compiling Rust library...")
	if err := b.runBuildCmd(worktreeDir, logFile, "cargo", "build", "--features", features, "--lib", "--release"); err != nil {
		return nil, fmt.Errorf("cargo build failed: %v", err)
	}

	// Step 3: FFI bindgen workaround
	writeLog("Step 3/4: Applying FFI bindgen workaround...")
	bridgeDart := filepath.Join(worktreeDir, "flutter", "lib", "generated_bridge.dart")
	_ = b.runBuildCmd(worktreeDir, logFile, "sed", "-i", // best-effort workaround
		"s/ffi.NativeFunction<ffi.Bool Function(DartPort/ffi.NativeFunction<ffi.Uint8 Function(DartPort/g",
		bridgeDart)

	// Step 4: Build Flutter
	_ = b.runBuildCmd(worktreeDir, logFile, "git", "checkout", "--", "flutter/pubspec.yaml") // best-effort restore
	writeLog("Step 4/4: Building Flutter UI...")
	if err := b.runBuildCmd(filepath.Join(worktreeDir, "flutter"), logFile, "flutter", "build", "linux", "--release"); err != nil {
		return nil, fmt.Errorf("flutter build failed: %v", err)
	}

	buildOutputDir := GetBuildOutputDir(worktreeDir, platform)
	if _, err := os.Stat(buildOutputDir); err != nil {
		return nil, fmt.Errorf("build output folder not found: %s", buildOutputDir)
	}
	writeLog("Build output folder: %s", buildOutputDir)

	return &BuildResult{OutputDir: buildOutputDir, LogPath: logPath}, nil
}

// Cancel terminates the currently running build command.
func (b *PreBuilder) Cancel() {
	b.mu.Lock()
	cmd := b.cancelCmd
	b.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

// GetLogContent reads build log from offset.
func (b *PreBuilder) GetLogContent(logPath string, offset int64) (string, int64, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, nil
		}
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", 0, err
		}
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return "", offset, err
	}
	return string(data), offset + int64(len(data)), nil
}

func (b *PreBuilder) ensureWorktree(srcDir, worktreeDir string, writeLog func(string, ...any)) error {
	absSrc, _ := filepath.Abs(srcDir)
	absWt, _ := filepath.Abs(worktreeDir)

	if _, err := os.Stat(absWt); err == nil {
		if err := b.runInDir(absWt, nil, "git", "status"); err != nil {
			writeLog("Existing worktree seems broken, removing and recreating...")
			_ = os.RemoveAll(absWt)
			_ = exec.Command("git", "-C", absSrc, "worktree", "remove", "--force", absWt).Run()
		} else {
			return nil
		}
	}

	writeLog("Creating build worktree at %s (source: %s)...", absWt, absSrc)
	if err := os.MkdirAll(filepath.Dir(absWt), 0755); err != nil {
		return fmt.Errorf("failed to create worktree parent dir: %w", err)
	}
	cmd := exec.Command("git", "-C", absSrc, "worktree", "add", "--detach", absWt)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add failed: %w\n%s", err, string(out))
	}
	return nil
}

func (b *PreBuilder) runBuildCmd(dir string, logWriter io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	b.mu.Lock()
	b.cancelCmd = cmd
	b.mu.Unlock()

	err := cmd.Run()

	b.mu.Lock()
	b.cancelCmd = nil
	b.mu.Unlock()

	return err
}

func (b *PreBuilder) runInDir(dir string, logWriter io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if logWriter != nil {
		cmd.Stdout = logWriter
		cmd.Stderr = logWriter
	}
	return cmd.Run()
}

func patchPublicKey(commonRsPath, pubKey string) error {
	data, err := os.ReadFile(commonRsPath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", commonRsPath, err)
	}
	re := regexp.MustCompile(`const KEY: &str = "[^"]+";`)
	if !re.MatchString(string(data)) {
		return fmt.Errorf("could not find KEY constant in %s", commonRsPath)
	}
	newContent := re.ReplaceAllString(string(data), fmt.Sprintf(`const KEY: &str = "%s";`, pubKey))
	return os.WriteFile(commonRsPath, []byte(newContent), 0644)
}

// GetBuildOutputDir returns the expected Flutter build output directory path.
func GetBuildOutputDir(worktreeDir, platform string) string {
	switch platform {
	case "linux":
		return filepath.Join(worktreeDir, "flutter", "build", "linux", "x64", "release", "bundle")
	case "windows":
		return filepath.Join(worktreeDir, "flutter", "build", "windows", "x64", "runner", "Release")
	case "macos":
		return filepath.Join(worktreeDir, "flutter", "build", "macos", "Build", "Products", "Release")
	default:
		return filepath.Join(worktreeDir, "flutter", "build")
	}
}
