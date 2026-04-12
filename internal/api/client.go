package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client communicates with the rustdesk-api worker endpoints.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// PlatformConfig describes a platform+arch pair that this worker supports.
type PlatformConfig struct {
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}

// WorkerJob is the task payload from the API server.
type WorkerJob struct {
	ID            uint   `json:"id"`
	Type          string `json:"type"` // "pre-build" or "bundle"
	Version       string `json:"version,omitempty"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch"`
	Format        string `json:"format,omitempty"`
	AppName       string `json:"app_name,omitempty"`
	CustomTxt     string `json:"custom_txt,omitempty"`
	ArtifactS3Key string `json:"artifact_s3_key,omitempty"`
	ArtifactDir   string `json:"artifact_dir,omitempty"`
}

// Register registers this worker with the API server.
func (c *Client) Register(name string, platforms any) error {
	resp, err := c.doRequest("POST", "/api/worker/register", map[string]any{
		"name": name, "platforms": platforms,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = decodeResponse(resp)
	return err
}

// Heartbeat sends a heartbeat to the API server.
func (c *Client) Heartbeat(name string) error {
	resp, err := c.doRequest("POST", "/api/worker/heartbeat", map[string]string{"name": name})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = decodeResponse(resp)
	return err
}

// PushVersions pushes the list of available pre-built versions to the API server.
func (c *Client) PushVersions(name string, versions []string) error {
	resp, err := c.doRequest("POST", "/api/worker/versions", map[string]any{
		"name": name, "versions": versions,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = decodeResponse(resp)
	return err
}

// FetchPendingJob polls for a pending job. Returns nil when no job is available.
func (c *Client) FetchPendingJob(name string, platforms []PlatformConfig) (*WorkerJob, error) {
	resp, err := c.doRequest("POST", "/api/worker/jobs/pending", map[string]any{
		"name": name, "platforms": platforms,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	r, err := decodeResponse(resp)
	if err != nil {
		return nil, err
	}

	var job WorkerJob
	if err := json.Unmarshal(r.Data, &job); err != nil {
		return nil, fmt.Errorf("decode job: %w", err)
	}
	return &job, nil
}

// IsJobCancelled checks if a job has been cancelled (status = failed with "cancelled by user").
func (c *Client) IsJobCancelled(jobID uint, jobType string) bool {
	resp, err := c.doRequest("GET", fmt.Sprintf("/api/worker/jobs/%d/status?type=%s", jobID, jobType), nil)
	if err != nil {
		return false // assume not cancelled on error
	}
	defer resp.Body.Close()

	r, err := decodeResponse(resp)
	if err != nil {
		return false
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(r.Data, &result); err != nil {
		return false
	}
	return result.Status == "failed"
}

// StartJob reports that a job has started.
func (c *Client) StartJob(jobID uint, jobType string) error {
	body := map[string]string{"type": jobType}
	resp, err := c.doRequest("POST", fmt.Sprintf("/api/worker/jobs/%d/start", jobID), body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// AppendLog sends log content to the API server.
func (c *Client) AppendLog(jobID uint, content string) error {
	body := map[string]string{"content": content}
	resp, err := c.doRequest("POST", fmt.Sprintf("/api/worker/jobs/%d/log", jobID), body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// CompleteJob reports job completion.
func (c *Client) CompleteJob(jobID uint, jobType, s3Key string, fileSize int64, logS3Key string) error {
	body := map[string]any{
		"type":       jobType,
		"s3_key":     s3Key,
		"file_size":  fileSize,
		"log_s3_key": logS3Key,
	}
	resp, err := c.doRequest("POST", fmt.Sprintf("/api/worker/jobs/%d/complete", jobID), body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// FailJob reports job failure.
func (c *Client) FailJob(jobID uint, jobType, errMsg string) error {
	body := map[string]string{
		"type":  jobType,
		"error": errMsg,
	}
	resp, err := c.doRequest("POST", fmt.Sprintf("/api/worker/jobs/%d/fail", jobID), body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) doRequest(method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return resp, nil
}

// apiResponse is the standard response wrapper from rustdesk-api.
type apiResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// decodeResponse checks the API response code and returns the data field.
func decodeResponse(resp *http.Response) (*apiResponse, error) {
	var r apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("API error (code=%d): %s", r.Code, r.Message)
	}
	return &r, nil
}
