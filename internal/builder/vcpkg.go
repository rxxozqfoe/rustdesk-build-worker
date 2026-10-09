package builder

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// vcpkgBaseline returns the vcpkg commit the release checked out in srcDir
// pins in its vcpkg.json: vcpkg-configuration.default-registry.baseline, or
// the older builtin-baseline.
func vcpkgBaseline(srcDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(srcDir, "vcpkg.json"))
	if err != nil {
		return "", fmt.Errorf("reading vcpkg.json: %w", err)
	}
	var m struct {
		Configuration struct {
			DefaultRegistry struct {
				Baseline string `json:"baseline"`
			} `json:"default-registry"`
		} `json:"vcpkg-configuration"`
		BuiltinBaseline string `json:"builtin-baseline"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("parsing vcpkg.json: %w", err)
	}
	baseline := m.Configuration.DefaultRegistry.Baseline
	if baseline == "" {
		baseline = m.BuiltinBaseline
	}
	if !commitRe.MatchString(baseline) {
		return "", fmt.Errorf("vcpkg.json has no valid baseline commit (got %q)", baseline)
	}
	return baseline, nil
}

// removeStaleLock deletes the index.lock a killed git process (say, a pod
// restarted mid-checkout) leaves behind. Callers hold gitMu and the worker
// runs one build at a time, so no live git process can own it.
func removeStaleLock(repoDir string) {
	_ = os.Remove(filepath.Join(repoDir, ".git", "index.lock"))
}

// prepareVcpkg brings the worker's vcpkg clone to the baseline the release in
// srcDir pins and installs that release's dependencies into
// <vcpkgDir>/installed. Packages built before come from the binary cache.
func (b *PreBuilder) prepareVcpkg(srcDir string, logFile io.Writer) error {
	baseline, err := vcpkgBaseline(srcDir)
	if err != nil {
		return err
	}
	vcpkgDir, _ := filepath.Abs(b.vcpkgDir)

	b.gitMu.Lock()
	err = b.syncVcpkg(vcpkgDir, baseline, logFile)
	b.gitMu.Unlock()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(vcpkgDir, "binary-cache"), 0755); err != nil {
		return fmt.Errorf("creating the vcpkg binary cache: %w", err)
	}
	if err := b.runBuildCmd(vcpkgDir, logFile, filepath.Join(vcpkgDir, "bootstrap-vcpkg.sh"), "-disableMetrics"); err != nil {
		return fmt.Errorf("vcpkg bootstrap failed: %v", err)
	}
	if err := b.runBuildCmd(srcDir, logFile, filepath.Join(vcpkgDir, "vcpkg"), "install",
		"--x-install-root="+filepath.Join(vcpkgDir, "installed"), "--clean-after-build"); err != nil {
		return fmt.Errorf("vcpkg install failed: %v", err)
	}
	return nil
}

// syncVcpkg clones vcpkgRepoURL into vcpkgDir when missing, fetches when the
// baseline is not there yet, and checks the baseline out. Callers hold gitMu.
func (b *PreBuilder) syncVcpkg(vcpkgDir, baseline string, logFile io.Writer) error {
	if _, err := os.Stat(filepath.Join(vcpkgDir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(vcpkgDir), 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", filepath.Dir(vcpkgDir), err)
		}
		if err := runGitRemote(b.vcpkgRepoURL, "", logFile, "clone", b.vcpkgRepoURL, vcpkgDir); err != nil {
			return fmt.Errorf("vcpkg clone %s failed: %w", redactURL(b.vcpkgRepoURL), err)
		}
	}
	removeStaleLock(vcpkgDir)
	if err := b.runInDir(vcpkgDir, nil, "git", "cat-file", "-e", baseline+"^{commit}"); err != nil {
		if err := runGitRemote(b.vcpkgRepoURL, vcpkgDir, logFile, "fetch", "origin"); err != nil {
			return fmt.Errorf("vcpkg fetch failed: %w", err)
		}
	}
	if err := b.runInDir(vcpkgDir, logFile, "git", "checkout", "--force", "--detach", baseline); err != nil {
		return fmt.Errorf("vcpkg checkout %s failed: %v", baseline, err)
	}
	return nil
}
