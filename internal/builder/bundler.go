package builder

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
	RustdeskSrcDir string // path to rustdesk source (for res/ files)
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
	resDir := filepath.Join(opts.RustdeskSrcDir, "res")

	// Create directory structure matching official deb
	for _, dir := range []string{
		filepath.Join(debRoot, "usr", "bin"),
		dataDir,
		filepath.Join(dataDir, "files", "systemd"),
		filepath.Join(debRoot, "usr", "share", "icons", "hicolor", "256x256", "apps"),
		filepath.Join(debRoot, "usr", "share", "icons", "hicolor", "scalable", "apps"),
		filepath.Join(debRoot, "usr", "share", "applications"),
		filepath.Join(debRoot, "usr", "share", "polkit-1", "actions"),
		filepath.Join(debRoot, "etc", "rustdesk"),
		filepath.Join(debRoot, "etc", "pam.d"),
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

	// Copy res files (icons, desktop entries, service, etc.)
	copyIfExists := func(src, dst string) {
		if _, err := os.Stat(src); err == nil {
			if err := exec.Command("cp", "-a", src, dst).Run(); err != nil {
				log.Printf("Warning: failed to copy %s to %s: %v", src, dst, err)
			}
		}
	}

	// Icons
	copyIfExists(filepath.Join(resDir, "128x128@2x.png"),
		filepath.Join(debRoot, "usr", "share", "icons", "hicolor", "256x256", "apps", "rustdesk.png"))
	copyIfExists(filepath.Join(resDir, "scalable.svg"),
		filepath.Join(debRoot, "usr", "share", "icons", "hicolor", "scalable", "apps", "rustdesk.svg"))

	// Desktop entries
	copyIfExists(filepath.Join(resDir, "rustdesk.desktop"),
		filepath.Join(debRoot, "usr", "share", "applications", "rustdesk.desktop"))
	copyIfExists(filepath.Join(resDir, "rustdesk-link.desktop"),
		filepath.Join(debRoot, "usr", "share", "applications", "rustdesk-link.desktop"))

	// Systemd service
	copyIfExists(filepath.Join(resDir, "rustdesk.service"),
		filepath.Join(dataDir, "files", "systemd", "rustdesk.service"))

	// PAM config
	copyIfExists(filepath.Join(resDir, "pam.d", "rustdesk.debian"),
		filepath.Join(debRoot, "etc", "pam.d", "rustdesk"))

	// startwm.sh, xorg.conf
	copyIfExists(filepath.Join(resDir, "startwm.sh"),
		filepath.Join(debRoot, "etc", "rustdesk", "startwm.sh"))
	copyIfExists(filepath.Join(resDir, "xorg.conf"),
		filepath.Join(debRoot, "etc", "rustdesk", "xorg.conf"))

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
Depends: libgtk-3-0, libxcb-randr0, libxdo3 | libxdo4, libxfixes3, libxcb-shape0, libxcb-xfixes0, libasound2, libsystemd0, curl, libva2, libva-drm2, libva-x11-2, libgstreamer-plugins-base1.0-0, libpam0g
Recommends: libayatana-appindicator3-1
Description: RustDesk - remote control software.

`, opts.Version, debArch(opts.Arch))

	if err := os.WriteFile(filepath.Join(debianDir, "control"), []byte(control), 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write control file: %w", err)
	}

	// Copy DEBIAN scripts (postinst, postrm, preinst, prerm) from res as-is
	debianResDir := filepath.Join(resDir, "DEBIAN")
	if entries, err := os.ReadDir(debianResDir); err == nil {
		for _, e := range entries {
			if e.Name() == "control" {
				continue // we generate our own
			}
			src := filepath.Join(debianResDir, e.Name())
			dst := filepath.Join(debianDir, e.Name())
			if data, err := os.ReadFile(src); err == nil {
				if err := os.WriteFile(dst, data, 0755); err != nil {
					cleanup()
					return nil, fmt.Errorf("failed to write DEBIAN script %s: %w", e.Name(), err)
				}
			}
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
