package builder

import (
	"fmt"
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

// Bundle takes a build output folder, injects custom.txt, and packages into the requested format.
func Bundle(buildDir, format, customTxtContent string) (*BundleResult, error) {
	switch format {
	case "deb":
		return packageDeb(buildDir, customTxtContent)
	case "zip":
		return packageZip(buildDir, customTxtContent)
	default:
		return nil, fmt.Errorf("packaging format not yet supported: %s", format)
	}
}

func packageDeb(buildDir, customTxtContent string) (*BundleResult, error) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		return nil, fmt.Errorf("dpkg-deb not found: %w", err)
	}

	workDir, err := os.MkdirTemp("", "rustdesk-repack-deb-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(workDir) }

	debRoot := filepath.Join(workDir, "deb")
	dataDir := filepath.Join(debRoot, "usr", "share", "rustdesk")
	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(filepath.Join(debRoot, "usr", "bin"), 0755)

	cmd := exec.Command("cp", "-a", buildDir+"/.", dataDir+"/")
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to copy build output: %w\n%s", err, string(out))
	}

	if err := os.WriteFile(filepath.Join(dataDir, "custom.txt"), []byte(customTxtContent), 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write custom.txt: %w", err)
	}

	os.Symlink("/usr/share/rustdesk/rustdesk", filepath.Join(debRoot, "usr", "bin", "rustdesk"))

	debianDir := filepath.Join(debRoot, "DEBIAN")
	os.MkdirAll(debianDir, 0755)
	control := `Package: rustdesk
Architecture: amd64
Version: 0.0.0
Depends: libgtk-3-0, libxcb-randr0, libxdo3 | libxdo4, libxfixes3, libxcb-shape0, libxcb-xfixes0, libasound2, libsystemd0, curl, libva2, libva-drm2, libva-x11-2, libpam0g
Description: RustDesk custom client
`
	if err := os.WriteFile(filepath.Join(debianDir, "control"), []byte(control), 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write control file: %w", err)
	}

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

func packageZip(buildDir, customTxtContent string) (*BundleResult, error) {
	if _, err := exec.LookPath("zip"); err != nil {
		return nil, fmt.Errorf("zip not found: %w", err)
	}

	workDir, err := os.MkdirTemp("", "rustdesk-repack-zip-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(workDir) }

	stageDir := filepath.Join(workDir, "rustdesk")
	cmd := exec.Command("cp", "-a", buildDir, stageDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to copy build output: %w\n%s", err, string(out))
	}

	if err := os.WriteFile(filepath.Join(stageDir, "custom.txt"), []byte(customTxtContent), 0644); err != nil {
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
