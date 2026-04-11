package worker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nicholaswilde/rustdesk-build-worker/internal/api"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/builder"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/config"
	"github.com/nicholaswilde/rustdesk-build-worker/internal/s3"
)

// Worker is the main polling loop that fetches jobs from the API server,
// executes builds/bundles, uploads results to S3, and reports back.
type Worker struct {
	cfg        *config.Config
	apiClient  *api.Client
	s3Client   *s3.Client
	preBuilder *builder.PreBuilder
	jobLogs    sync.Map // job ID (uint) → log file path (string)
}

func New(cfg *config.Config, apiClient *api.Client, s3Client *s3.Client) *Worker {
	return &Worker{
		cfg:       cfg,
		apiClient: apiClient,
		s3Client:  s3Client,
		preBuilder: builder.NewPreBuilder(
			cfg.Build.RustdeskSrcDir,
			cfg.Build.WorktreeDir,
			cfg.Build.LogDir,
		),
	}
}

// PreBuilder returns the pre-builder for HTTP handlers (versions, log).
func (w *Worker) PreBuilder() *builder.PreBuilder {
	return w.preBuilder
}

// Run starts the polling loop. Blocks until context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Println("Worker polling loop started")
	for {
		select {
		case <-ctx.Done():
			log.Println("Worker shutting down")
			return
		default:
		}

		job, err := w.apiClient.FetchPendingJob()
		if err != nil {
			log.Printf("Error fetching job: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if job == nil {
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("Got job: id=%d type=%s", job.ID, job.Type)

		switch job.Type {
		case "pre-build":
			w.handlePreBuild(job)
		case "bundle":
			w.handleBundle(job)
		default:
			log.Printf("Unknown job type: %s", job.Type)
			w.apiClient.FailJob(job.ID, job.Type, "unknown job type: "+job.Type)
		}
	}
}

func (w *Worker) handlePreBuild(job *api.WorkerJob) {
	if err := w.apiClient.StartJob(job.ID, "pre-build"); err != nil {
		log.Printf("Failed to start job %d: %v", job.ID, err)
		return
	}

	pubKey := w.cfg.Build.SigningPublicKey

	result, err := w.preBuilder.Build(job.Version, job.Platform, job.Arch, pubKey)
	if err != nil {
		log.Printf("Pre-build failed: %v", err)
		w.apiClient.FailJob(job.ID, "pre-build", err.Error())
		return
	}

	// Track log path for HTTP handler and send to API
	if result.LogPath != "" {
		w.jobLogs.Store(job.ID, result.LogPath)
		if logContent, err := os.ReadFile(result.LogPath); err == nil {
			if err := w.apiClient.AppendLog(job.ID, string(logContent)); err != nil {
				log.Printf("Warning: failed to send log for job %d: %v", job.ID, err)
			}
		}
	}

	// Tar.gz the build output and upload to S3
	s3Key := fmt.Sprintf("pre-builds/%s-%s-%s.tar.gz", job.Platform, job.Arch, job.Version)
	tarPath := filepath.Join(os.TempDir(), fmt.Sprintf("prebuild-%d.tar.gz", job.ID))
	defer os.Remove(tarPath)

	if err := createTarGz(tarPath, result.OutputDir); err != nil {
		log.Printf("Failed to create tar.gz: %v", err)
		w.apiClient.FailJob(job.ID, "pre-build", "tar.gz creation failed: "+err.Error())
		return
	}

	if _, err := w.s3Client.UploadFile(context.Background(), s3Key, tarPath, "application/gzip"); err != nil {
		log.Printf("S3 upload failed: %v", err)
		w.apiClient.FailJob(job.ID, "pre-build", "S3 upload failed: "+err.Error())
		return
	}

	log.Printf("Pre-build completed, S3 key: %s", s3Key)
	w.apiClient.CompleteJob(job.ID, "pre-build", s3Key, 0)
}

func (w *Worker) handleBundle(job *api.WorkerJob) {
	// Download the pre-build artifact from S3 (or use local dir)
	var buildDir string
	if job.ArtifactS3Key != "" {
		// Download from S3
		tmpDir, err := os.MkdirTemp("", "bundle-artifact-*")
		if err != nil {
			w.apiClient.FailJob(job.ID, "bundle", "failed to create temp dir: "+err.Error())
			return
		}
		defer os.RemoveAll(tmpDir)

		tarPath := filepath.Join(tmpDir, "artifact.tar.gz")
		if err := w.s3Client.DownloadFile(context.Background(), job.ArtifactS3Key, tarPath); err != nil {
			w.apiClient.FailJob(job.ID, "bundle", "S3 download failed: "+err.Error())
			return
		}

		if err := extractTarGz(tarPath, tmpDir); err != nil {
			w.apiClient.FailJob(job.ID, "bundle", "tar.gz extraction failed: "+err.Error())
			return
		}
		os.Remove(tarPath)

		// Find the extracted directory (should be a single subdirectory)
		entries, _ := os.ReadDir(tmpDir)
		if len(entries) == 0 {
			w.apiClient.FailJob(job.ID, "bundle", "no files in extracted artifact")
			return
		}
		buildDir = filepath.Join(tmpDir, entries[0].Name())
	} else if job.ArtifactDir != "" {
		buildDir = job.ArtifactDir
	} else {
		w.apiClient.FailJob(job.ID, "bundle", "no artifact source specified")
		return
	}

	// Bundle
	result, err := builder.Bundle(buildDir, job.Format, job.CustomTxt)
	if err != nil {
		w.apiClient.FailJob(job.ID, "bundle", "bundling failed: "+err.Error())
		return
	}
	defer result.Cleanup()

	// Upload to S3
	appName := job.AppName
	if appName == "" {
		appName = "rustdesk"
	}
	version := job.Version
	if version == "" {
		version = "0.0.0"
	}
	filename := fmt.Sprintf("%s-%s-%s-%s.%s", appName, version, job.Platform, job.Arch, job.Format)
	s3Key := fmt.Sprintf("bundles/%d-%s", job.ID, filename)

	contentType := "application/octet-stream"
	switch job.Format {
	case "deb":
		contentType = "application/vnd.debian.binary-package"
	case "zip":
		contentType = "application/zip"
	}

	if _, err := w.s3Client.UploadFile(context.Background(), s3Key, result.FilePath, contentType); err != nil {
		w.apiClient.FailJob(job.ID, "bundle", "S3 upload failed: "+err.Error())
		return
	}

	log.Printf("Bundle completed, S3 key: %s", s3Key)
	w.apiClient.CompleteJob(job.ID, "bundle", s3Key, result.FileSize)
}

func createTarGz(outputPath, sourceDir string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	baseDir := filepath.Base(sourceDir)
	return filepath.WalkDir(sourceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Use Lstat to detect symlinks (Walk/WalkDir follows them)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(sourceDir, path)
		name := filepath.Join(baseDir, rel)

		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header := &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     name,
				Linkname: link,
			}
			return tw.WriteHeader(header)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !info.IsDir() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			_, err = io.Copy(tw, file)
			return err
		}
		return nil
	})
}

func extractTarGz(tarPath, destDir string) error {
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)

	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)
		// Prevent path traversal
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) &&
			filepath.Clean(target) != filepath.Clean(destDir) {
			return fmt.Errorf("tar entry %q escapes destination directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			os.Chmod(target, os.FileMode(header.Mode)&0777) // mask setuid/setgid
		case tar.TypeSymlink:
			// Validate symlink target stays within destination
			linkTarget := header.Linkname
			if !filepath.IsAbs(linkTarget) {
				linkTarget = filepath.Join(filepath.Dir(target), linkTarget)
			}
			if !strings.HasPrefix(filepath.Clean(linkTarget)+string(os.PathSeparator), cleanDest) &&
				filepath.Clean(linkTarget) != filepath.Clean(destDir) {
				return fmt.Errorf("tar symlink %q points outside destination: %s", header.Name, header.Linkname)
			}
			os.MkdirAll(filepath.Dir(target), 0755)
			os.Symlink(header.Linkname, target)
		}
	}
	return nil
}
