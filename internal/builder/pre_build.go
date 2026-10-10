package builder

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Options configures a PreBuilder. SrcDir and VcpkgDir are clones the worker
// owns: created from their repo URLs when missing and reset by every build,
// so neither may be a checkout someone works in.
type Options struct {
	SrcDir       string
	RepoURL      string
	VcpkgDir     string
	VcpkgRepoURL string
	LogDir       string
	// Jobs bounds build parallelism (CARGO_BUILD_JOBS and
	// VCPKG_MAX_CONCURRENCY); 0 leaves the tools' defaults.
	Jobs int
}

// PreBuilder handles compiling RustDesk from source.
type PreBuilder struct {
	srcDir       string
	repoURL      string
	vcpkgDir     string
	vcpkgRepoURL string
	logDir       string
	jobs         int

	mu        sync.Mutex
	cancelCmd *exec.Cmd

	// gitMu serialises clone/fetch/checkout in srcDir and vcpkgDir between
	// the build and the periodic version refresh.
	gitMu sync.Mutex
}

func NewPreBuilder(o Options) *PreBuilder {
	return &PreBuilder{
		srcDir:       o.SrcDir,
		repoURL:      o.RepoURL,
		vcpkgDir:     o.VcpkgDir,
		vcpkgRepoURL: o.VcpkgRepoURL,
		logDir:       o.LogDir,
		jobs:         o.Jobs,
	}
}

// BuildResult contains the output of a successful build.
type BuildResult struct {
	OutputDir string // path to the build output folder
	LogPath   string // path to the build log
}

var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+`)

// versionRe is what a version handed to git must look like: a release tag,
// never something git could read as an option.
var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+[0-9A-Za-z.+-]*$`)

func checkVersion(version string) error {
	if !versionRe.MatchString(version) {
		return fmt.Errorf("invalid version %q", version)
	}
	return nil
}

// redactURL hides any credentials in a repository URL, which would
// otherwise reach the build log (uploaded to S3) and job errors.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<repository>"
	}
	if u.User != nil {
		u.User = url.User("REDACTED")
	}
	return u.String()
}

// runGitRemote runs a git command that talks to repoURL. Its output can echo
// the URL, so it is written to logWriter only with the URL redacted.
func runGitRemote(repoURL, dir string, logWriter io.Writer, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if logWriter != nil && len(out) > 0 {
		_, _ = io.WriteString(logWriter, strings.ReplaceAll(string(out), repoURL, redactURL(repoURL)))
	}
	return err
}

// syncSource clones repoURL into srcDir when it is not a git repository yet,
// otherwise fetches new commits and tags. Callers hold gitMu.
func (b *PreBuilder) syncSource(logWriter io.Writer) error {
	srcDir, _ := filepath.Abs(b.srcDir)
	if _, err := os.Stat(filepath.Join(srcDir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(srcDir), 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", filepath.Dir(srcDir), err)
		}
		if err := runGitRemote(b.repoURL, "", logWriter, "clone", b.repoURL, srcDir); err != nil {
			return fmt.Errorf("git clone %s failed: %w", redactURL(b.repoURL), err)
		}
		return nil
	}
	removeStaleLocks(srcDir)
	if err := setOrigin(srcDir, b.repoURL); err != nil {
		return err
	}
	if err := runGitRemote(b.repoURL, srcDir, logWriter, "fetch", "origin", "--tags", "--force"); err != nil {
		return fmt.Errorf("git fetch failed: %w", err)
	}
	return nil
}

// ListVersions fetches the source tree, cloning it on first use, and returns
// its release tags, newest first. A failed fetch still lists the tags already
// known.
func (b *PreBuilder) ListVersions() ([]string, error) {
	srcDir, _ := filepath.Abs(b.srcDir)

	// git's (redacted) output goes to the worker log, so a failed first
	// clone says why.
	b.gitMu.Lock()
	err := b.syncSource(log.Writer())
	b.gitMu.Unlock()
	if err != nil {
		log.Printf("Warning: syncing %s: %v", srcDir, err)
	}

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
// onLog, when set, receives the path of this build's log as soon as the file
// exists, so the caller can stream it while the build runs.
func (b *PreBuilder) Build(version, platform, arch, pubKey string, onLog func(logPath string)) (*BuildResult, error) {
	if platform != "linux" {
		return nil, fmt.Errorf("only linux platform is supported for builds")
	}
	if arch != "x86_64" && arch != "aarch64" {
		return nil, fmt.Errorf("unsupported architecture: %s", arch)
	}
	if err := checkVersion(version); err != nil {
		return nil, err
	}

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
	if onLog != nil {
		onLog(logPath)
	}

	logger := bufio.NewWriter(logFile)
	writeLog := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		_, _ = fmt.Fprintf(logger, "[%s] %s\n", time.Now().Format("15:04:05"), msg)
		_ = logger.Flush()
	}

	writeLog("Pre-build started: version=%s platform=%s arch=%s", version, platform, arch)

	writeLog("Syncing %s from %s and checking out version %s...", srcDir, redactURL(b.repoURL), version)
	if err := b.checkout(version, logFile); err != nil {
		return nil, err
	}

	if pubKey != "" {
		writeLog("Patching signing public key in common.rs...")
		commonRsPath := filepath.Join(srcDir, "src", "common.rs")
		if err := patchPublicKey(commonRsPath, pubKey); err != nil {
			return nil, fmt.Errorf("failed to patch public key: %v", err)
		}
	}

	vcpkgDir, _ := filepath.Abs(b.vcpkgDir)
	writeLog("Preparing vcpkg dependencies in %s from %s...", vcpkgDir, redactURL(b.vcpkgRepoURL))
	if err := b.prepareVcpkg(srcDir, logFile); err != nil {
		return nil, err
	}

	if _, err := exec.LookPath("cargo"); err != nil {
		return nil, fmt.Errorf("cargo not found in PATH")
	}
	if _, err := exec.LookPath("flutter"); err != nil {
		return nil, fmt.Errorf("flutter not found in PATH")
	}

	// Step 1: flutter_rust_bridge codegen
	writeLog("Step 1/4: Generating flutter_rust_bridge code...")
	if err := b.runBuildCmd(filepath.Join(srcDir, "flutter"), logFile, "flutter", "pub", "get"); err != nil {
		return nil, fmt.Errorf("flutter pub get failed: %v", err)
	}
	if err := b.runBuildCmd(srcDir, logFile,
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
	if err := b.runBuildCmd(srcDir, logFile, "cargo", "build", "--features", features, "--lib", "--release"); err != nil {
		return nil, fmt.Errorf("cargo build failed: %v", err)
	}

	// Step 3: FFI bindgen workaround
	writeLog("Step 3/4: Applying FFI bindgen workaround...")
	bridgeDart := filepath.Join(srcDir, "flutter", "lib", "generated_bridge.dart")
	_ = b.runBuildCmd(srcDir, logFile, "sed", "-i", // best-effort workaround
		"s/ffi.NativeFunction<ffi.Bool Function(DartPort/ffi.NativeFunction<ffi.Uint8 Function(DartPort/g",
		bridgeDart)

	// Step 4: Build Flutter
	writeLog("Step 4/4: Building Flutter UI...")
	if err := b.runBuildCmd(filepath.Join(srcDir, "flutter"), logFile, "flutter", "build", "linux", "--release"); err != nil {
		return nil, fmt.Errorf("flutter build failed: %v", err)
	}

	buildOutputDir := GetBuildOutputDir(srcDir, platform)
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

// checkout brings srcDir to `version`, with its submodules, discarding the
// previous build's patches and untracked files.
func (b *PreBuilder) checkout(version string, logFile io.Writer) error {
	srcDir, _ := filepath.Abs(b.srcDir)

	b.gitMu.Lock()
	defer b.gitMu.Unlock()

	if err := b.syncSource(logFile); err != nil {
		return err
	}
	if err := b.runInDir(srcDir, logFile, "git", "checkout", "--force", version); err != nil {
		return fmt.Errorf("git checkout %s failed: %v", version, err)
	}
	_ = b.runInDir(srcDir, logFile, "git", "clean", "-fd") // best-effort cleanup
	if err := b.runInDir(srcDir, logFile, "git", "submodule", "update", "--init", "--recursive", "--force"); err != nil {
		return fmt.Errorf("git submodule update failed: %v", err)
	}
	return nil
}

// buildEnv is the environment of every build command: the worker's own, with
// VCPKG_ROOT at the worker's vcpkg clone and, when Jobs is set, the build
// parallelism. Later entries win, so these override inherited values.
func (b *PreBuilder) buildEnv() []string {
	vcpkgDir, _ := filepath.Abs(b.vcpkgDir)
	env := append(os.Environ(),
		"VCPKG_ROOT="+vcpkgDir,
		"VCPKG_DEFAULT_BINARY_CACHE="+filepath.Join(vcpkgDir, "binary-cache"),
		"VCPKG_DISABLE_METRICS=1",
	)
	if b.jobs > 0 {
		env = append(env,
			fmt.Sprintf("CARGO_BUILD_JOBS=%d", b.jobs),
			fmt.Sprintf("VCPKG_MAX_CONCURRENCY=%d", b.jobs),
		)
	}
	return env
}

func (b *PreBuilder) runBuildCmd(dir string, logWriter io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = b.buildEnv()
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
func GetBuildOutputDir(srcDir, platform string) string {
	switch platform {
	case "linux":
		return filepath.Join(srcDir, "flutter", "build", "linux", "x64", "release", "bundle")
	case "windows":
		return filepath.Join(srcDir, "flutter", "build", "windows", "x64", "runner", "Release")
	case "macos":
		return filepath.Join(srcDir, "flutter", "build", "macos", "Build", "Products", "Release")
	default:
		return filepath.Join(srcDir, "flutter", "build")
	}
}
