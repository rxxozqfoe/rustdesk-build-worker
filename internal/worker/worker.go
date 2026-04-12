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

// Run registers the worker, starts background goroutines, and enters the polling loop.
// Blocks until context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	// 1. Register
	log.Printf("Registering worker %q...", w.cfg.Worker.Name)
	if err := w.apiClient.Register(w.cfg.Worker.Name, w.cfg.Worker.Platforms); err != nil {
		log.Fatalf("Failed to register: %v", err)
	}
	log.Printf("Registered as %q with platforms: %v", w.cfg.Worker.Name, w.cfg.Worker.Platforms)

	// 2. Push initial versions
	if versions, err := w.preBuilder.ListVersions(); err == nil && len(versions) > 0 {
		if err := w.apiClient.PushVersions(w.cfg.Worker.Name, versions); err != nil {
			log.Printf("Warning: failed to push versions: %v", err)
		} else {
			log.Printf("Pushed %d versions", len(versions))
		}
	}

	// 3. Start heartbeat goroutine (every 5s, timeout is 15s on API side)
	go w.heartbeatLoop(ctx)

	// 4. Start version refresh goroutine (every 5 minutes)
	go w.versionPushLoop(ctx)

	// 5. Polling loop
	log.Println("Worker polling loop started")
	w.pollLoop(ctx)
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.apiClient.Heartbeat(w.cfg.Worker.Name); err != nil {
				log.Printf("Heartbeat failed: %v", err)
			}
		}
	}
}

func (w *Worker) versionPushLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if versions, err := w.preBuilder.ListVersions(); err == nil {
				w.apiClient.PushVersions(w.cfg.Worker.Name, versions)
			}
		}
	}
}

func (w *Worker) pollLoop(ctx context.Context) {
	platforms := make([]api.PlatformConfig, len(w.cfg.Worker.Platforms))
	for i, p := range w.cfg.Worker.Platforms {
		platforms[i] = api.PlatformConfig{
			Platform: p.Platform,
			Arch:     p.Arch,
		}
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Worker shutting down")
			return
		default:
		}

		job, err := w.apiClient.FetchPendingJob(w.cfg.Worker.Name, platforms)
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

// streamLog tails a log file and pushes incremental content to the API every 3 seconds.
// Also checks for job cancellation. Returns a cancel function and a channel
// that receives true if the job was cancelled.
func (w *Worker) streamLog(jobID uint, jobType, logPath string) (context.CancelFunc, <-chan bool) {
	ctx, cancel := context.WithCancel(context.Background())
	cancelledCh := make(chan bool, 1)
	go func() {
		var offset int64
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Final flush
				w.pushLogIncrement(jobID, logPath, &offset)
				return
			case <-ticker.C:
				w.pushLogIncrement(jobID, logPath, &offset)
				// Check if job was cancelled
				if w.apiClient.IsJobCancelled(jobID, jobType) {
					cancelledCh <- true
					return
				}
			}
		}
	}()
	return cancel, cancelledCh
}

func (w *Worker) pushLogIncrement(jobID uint, logPath string, offset *int64) {
	f, err := os.Open(logPath)
	if err != nil {
		return
	}
	defer f.Close()

	if *offset > 0 {
		f.Seek(*offset, io.SeekStart)
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return
	}
	*offset += int64(len(data))
	w.apiClient.AppendLog(jobID, string(data))
}

func (w *Worker) handlePreBuild(job *api.WorkerJob) {
	if err := w.apiClient.StartJob(job.ID, "pre-build"); err != nil {
		log.Printf("Failed to start job %d: %v", job.ID, err)
		return
	}

	pubKey := w.cfg.Build.SigningPublicKey

	// Start streaming log before build begins — the builder writes to logDir
	// We'll figure out the log path after build, but we can predict it from the builder's pattern
	// Instead, start streaming after build starts (builder creates the file immediately)
	// We use a channel: build runs synchronously, log streams in background

	// Build runs synchronously — log file is created at the start of Build()
	// We need the log path first. The builder creates it deterministically.
	// Approach: start build, then stream. But build is blocking.
	// Better: run build in goroutine, stream from known log dir.

	// Run build in a goroutine so we can stream logs concurrently
	type buildResult struct {
		result *builder.BuildResult
		err    error
	}
	resultCh := make(chan buildResult, 1)
	go func() {
		r, err := w.preBuilder.Build(job.Version, job.Platform, job.Arch, pubKey)
		resultCh <- buildResult{r, err}
	}()

	// Wait briefly for the log file to be created, then start streaming
	var logPath string
	time.Sleep(500 * time.Millisecond)
	// Find the most recent log file in the log dir
	entries, _ := os.ReadDir(w.cfg.Build.LogDir)
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		if strings.HasPrefix(name, "prebuild_") && strings.HasSuffix(name, ".log") {
			logPath = filepath.Join(w.cfg.Build.LogDir, name)
			break
		}
	}

	var stopStream context.CancelFunc
	var cancelledCh <-chan bool
	if logPath != "" {
		stopStream, cancelledCh = w.streamLog(job.ID, "pre-build", logPath)
	}

	// Wait for build to complete or cancellation
	var br buildResult
	select {
	case br = <-resultCh:
		// Build finished normally
	case <-cancelledCh:
		// Job was cancelled — kill the build process
		log.Printf("Job %d cancelled by user, aborting build", job.ID)
		w.preBuilder.Cancel()
		<-resultCh // wait for build goroutine to exit
		if stopStream != nil {
			stopStream()
		}
		return
	}

	// Stop streaming (final flush)
	if stopStream != nil {
		stopStream()
		time.Sleep(100 * time.Millisecond) // let final flush complete
	}

	if br.err != nil {
		log.Printf("Pre-build failed: %v", br.err)
		w.apiClient.FailJob(job.ID, "pre-build", br.err.Error())
		return
	}
	result := br.result

	if result.LogPath != "" {
		w.jobLogs.Store(job.ID, result.LogPath)
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

	// Upload log to S3
	var logS3Key string
	if result.LogPath != "" {
		logS3Key = fmt.Sprintf("logs/prebuild-%d.log", job.ID)
		if _, err := w.s3Client.UploadFile(context.Background(), logS3Key, result.LogPath, "text/plain"); err != nil {
			log.Printf("Warning: log S3 upload failed: %v", err)
			logS3Key = "" // don't fail the job for log upload failure
		}
	}

	log.Printf("Pre-build completed, S3 key: %s", s3Key)
	w.apiClient.CompleteJob(job.ID, "pre-build", s3Key, 0, logS3Key)
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
	w.apiClient.CompleteJob(job.ID, "bundle", s3Key, result.FileSize, "")
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
