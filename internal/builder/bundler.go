package builder

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// BundleResult holds the path to the bundled file and a cleanup function.
type BundleResult struct {
	FilePath string
	FileSize int64
	Cleanup  func()
}

// BundleOptions contains metadata needed for packaging.
type BundleOptions struct {
	BuildDir       string // path to the build output folder
	Format         string // deb, zip
	CustomTxt      string // signed custom.txt content
	Version        string // version string for the package
	Arch           string // x86_64, aarch64
	RustdeskSrcDir string // the worker's rustdesk clone; res/ and build.py are read at Version
}

// srcAt reads files of one release from the rustdesk clone through git, so a
// bundle gets that release's packaging files whatever the clone has checked
// out at the time.
type srcAt struct {
	dir, version string
}

// read returns the file at path and whether it is executable;
// os.ErrNotExist when the release has no such file.
func (s srcAt) read(path string) ([]byte, bool, error) {
	out, err := exec.Command("git", "-C", s.dir, "ls-tree", s.version, "--", path).Output()
	if err != nil {
		return nil, false, fmt.Errorf("git ls-tree %s %s: %w", s.version, path, err)
	}
	// "<mode> blob <object>\t<path>"
	fields := strings.Fields(string(out))
	if len(fields) < 3 || fields[1] != "blob" {
		return nil, false, os.ErrNotExist
	}
	data, err := exec.Command("git", "-C", s.dir, "cat-file", "blob", fields[2]).Output()
	if err != nil {
		return nil, false, fmt.Errorf("git cat-file %s: %w", path, err)
	}
	return data, fields[0] == "100755", nil
}

// list returns the paths of the files directly under dir.
func (s srcAt) list(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", s.dir, "ls-tree", "--name-only", s.version, "--", dir+"/").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s %s: %w", s.version, dir, err)
	}
	return strings.Fields(string(out)), nil
}

var dependsRe = regexp.MustCompile(`(?m)^Depends: (.+)$`)

// debDepends returns the Depends line of the release's own deb, from
// generate_control_file in its build.py.
func (s srcAt) debDepends() (string, error) {
	data, _, err := s.read("build.py")
	if err != nil {
		return "", fmt.Errorf("reading build.py: %w", err)
	}
	m := dependsRe.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("no Depends line in build.py at %s", s.version)
	}
	// The %s is for extra armhf depends, which we do not build.
	return strings.TrimSpace(strings.ReplaceAll(string(m[1]), "%s", "")), nil
}

// Bundle takes a build output folder, injects custom.txt, and packages into the requested format.
func Bundle(opts BundleOptions) (*BundleResult, error) {
	switch opts.Format {
	case "deb":
		return packageDeb(opts)
	case "zip":
		return packageZip(opts)
	default:
		return nil, fmt.Errorf("packaging format not yet supported: %s", opts.Format)
	}
}

func debArch(arch string) string {
	switch arch {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return arch
	}
}

func packageDeb(opts BundleOptions) (*BundleResult, error) {
	if err := checkVersion(opts.Version); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		return nil, fmt.Errorf("dpkg-deb not found: %w", err)
	}

	workDir, err := os.MkdirTemp("", "rustdesk-repack-deb-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(workDir) }

	debRoot := filepath.Join(workDir, "deb")
	dataDir := filepath.Join(debRoot, "usr", "share", "rustdesk")
	src := srcAt{dir: opts.RustdeskSrcDir, version: opts.Version}

	depends, err := src.debDepends()
	if err != nil {
		cleanup()
		return nil, err
	}

	// Create directory structure matching official deb
	for _, dir := range []string{
		filepath.Join(debRoot, "usr", "bin"),
		dataDir,
		filepath.Join(dataDir, "files", "systemd"),
		filepath.Join(debRoot, "usr", "share", "icons", "hicolor", "256x256", "apps"),
		filepath.Join(debRoot, "usr", "share", "icons", "hicolor", "scalable", "apps"),
		filepath.Join(debRoot, "usr", "share", "applications"),
		filepath.Join(debRoot, "usr", "share", "polkit-1", "actions"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			cleanup()
			return nil, fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}

	// Copy build output
	cmd := exec.Command("cp", "-a", opts.BuildDir+"/.", dataDir+"/")
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to copy build output: %w\n%s", err, string(out))
	}

	// Inject custom.txt
	if err := os.WriteFile(filepath.Join(dataDir, "custom.txt"), []byte(opts.CustomTxt), 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write custom.txt: %w", err)
	}

	// Symlink: /usr/bin/rustdesk -> /usr/share/rustdesk/rustdesk
	if err := os.Symlink("/usr/share/rustdesk/rustdesk", filepath.Join(debRoot, "usr", "bin", "rustdesk")); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to create rustdesk symlink: %w", err)
	}

	// Copy res files (icons, desktop entries, service, etc.) of this release.
	// Files a release does not have are skipped: 1.5.0 dropped the PAM
	// config, startwm.sh and xorg.conf.
	copyRes := func(path, dst string, mode os.FileMode) error {
		data, executable, err := src.read(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if mode == 0 {
			mode = 0644
			if executable {
				mode = 0755
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, mode)
	}

	resFiles := []struct{ path, dst string }{
		// Icons
		{"res/128x128@2x.png", filepath.Join(debRoot, "usr", "share", "icons", "hicolor", "256x256", "apps", "rustdesk.png")},
		{"res/scalable.svg", filepath.Join(debRoot, "usr", "share", "icons", "hicolor", "scalable", "apps", "rustdesk.svg")},
		// Desktop entries
		{"res/rustdesk.desktop", filepath.Join(debRoot, "usr", "share", "applications", "rustdesk.desktop")},
		{"res/rustdesk-link.desktop", filepath.Join(debRoot, "usr", "share", "applications", "rustdesk-link.desktop")},
		// Systemd service
		{"res/rustdesk.service", filepath.Join(dataDir, "files", "systemd", "rustdesk.service")},
		// PAM config, startwm.sh, xorg.conf (before 1.5.0)
		{"res/pam.d/rustdesk.debian", filepath.Join(debRoot, "etc", "pam.d", "rustdesk")},
		{"res/startwm.sh", filepath.Join(debRoot, "etc", "rustdesk", "startwm.sh")},
		{"res/xorg.conf", filepath.Join(debRoot, "etc", "rustdesk", "xorg.conf")},
	}
	for _, f := range resFiles {
		if err := copyRes(f.path, f.dst, 0); err != nil {
			cleanup()
			return nil, fmt.Errorf("failed to copy %s: %w", f.path, err)
		}
	}

	// Polkit helper
	if err := os.WriteFile(filepath.Join(dataDir, "files", "polkit"), []byte("#!/bin/sh\n"), 0755); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write polkit helper: %w", err)
	}

	// DEBIAN control
	debianDir := filepath.Join(debRoot, "DEBIAN")
	if err := os.MkdirAll(debianDir, 0755); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to create DEBIAN dir: %w", err)
	}

	control := fmt.Sprintf(`Package: rustdesk
Section: net
Priority: optional
Version: %s
Architecture: %s
Maintainer: rustdesk <info@rustdesk.com>
Homepage: https://rustdesk.com
Depends: %s
Recommends: libayatana-appindicator3-1
Description: RustDesk - remote control software.

`, opts.Version, debArch(opts.Arch), depends)

	if err := os.WriteFile(filepath.Join(debianDir, "control"), []byte(control), 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write control file: %w", err)
	}

	// Copy DEBIAN scripts (postinst, postrm, preinst, prerm) from res as-is
	scripts, err := src.list("res/DEBIAN")
	if err != nil {
		cleanup()
		return nil, err
	}
	for _, path := range scripts {
		name := filepath.Base(path)
		if name == "control" {
			continue // we generate our own
		}
		if err := copyRes(path, filepath.Join(debianDir, name), 0755); err != nil {
			cleanup()
			return nil, fmt.Errorf("failed to write DEBIAN script %s: %w", name, err)
		}
	}

	// Build the deb
	outputFile := filepath.Join(workDir, "output.deb")
	cmd = exec.Command("dpkg-deb", "-b", debRoot, outputFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("dpkg-deb build failed: %w\n%s", err, string(out))
	}

	info, _ := os.Stat(outputFile)
	var fileSize int64
	if info != nil {
		fileSize = info.Size()
	}

	return &BundleResult{FilePath: outputFile, FileSize: fileSize, Cleanup: cleanup}, nil
}

func packageZip(opts BundleOptions) (*BundleResult, error) {
	if _, err := exec.LookPath("zip"); err != nil {
		return nil, fmt.Errorf("zip not found: %w", err)
	}

	workDir, err := os.MkdirTemp("", "rustdesk-repack-zip-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(workDir) }

	stageDir := filepath.Join(workDir, "rustdesk")
	cmd := exec.Command("cp", "-a", opts.BuildDir, stageDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to copy build output: %w\n%s", err, string(out))
	}

	if err := os.WriteFile(filepath.Join(stageDir, "custom.txt"), []byte(opts.CustomTxt), 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write custom.txt: %w", err)
	}

	outputFile := filepath.Join(workDir, "output.zip")
	cmd = exec.Command("zip", "-r", outputFile, "rustdesk")
	cmd.Dir = workDir
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("zip failed: %w\n%s", err, string(out))
	}

	info, _ := os.Stat(outputFile)
	var fileSize int64
	if info != nil {
		fileSize = info.Size()
	}

	return &BundleResult{FilePath: outputFile, FileSize: fileSize, Cleanup: cleanup}, nil
}
