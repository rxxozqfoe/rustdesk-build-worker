package worker

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ServeHTTP starts the worker's HTTP server for version listing and log proxying.
func (w *Worker) ServeHTTP(addr, token string) error {
	mux := http.NewServeMux()

	// Auth middleware
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(rw, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			next(rw, r)
		}
	}

	mux.HandleFunc("GET /api/worker/versions", auth(w.handleVersions))
	mux.HandleFunc("GET /api/worker/jobs/{id}/log", auth(w.handleLog))

	return http.ListenAndServe(addr, mux)
}

func (w *Worker) handleVersions(rw http.ResponseWriter, r *http.Request) {
	versions, err := w.preBuilder.ListVersions()
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"data": versions})
}

func (w *Worker) handleLog(rw http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")

	offsetStr := r.URL.Query().Get("offset")
	offset, _ := strconv.ParseInt(offsetStr, 10, 64)

	// Find log file matching this job ID
	logPath := w.findLogFile(idStr)
	if logPath == "" {
		writeJSON(rw, http.StatusOK, map[string]any{
			"data": map[string]any{"content": "", "new_offset": offset},
		})
		return
	}
	content, newOffset, err := w.preBuilder.GetLogContent(logPath, offset)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"data": map[string]any{
			"content":    content,
			"new_offset": newOffset,
		},
	})
}

// findLogFile looks up the log file path for a given job ID.
func (w *Worker) findLogFile(idStr string) string {
	// Check the in-memory map first
	id, _ := strconv.ParseUint(idStr, 10, 64)
	if v, ok := w.jobLogs.Load(uint(id)); ok {
		return v.(string)
	}

	// Fallback: scan log directory for files containing the job ID
	entries, err := os.ReadDir(w.cfg.Build.LogDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), idStr) {
			return filepath.Join(w.cfg.Build.LogDir, e.Name())
		}
	}
	return ""
}

func writeJSON(rw http.ResponseWriter, status int, data any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	b, _ := json.Marshal(data)
	rw.Write(b)
}
