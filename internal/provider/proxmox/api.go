package proxmox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

type apiError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("proxmox API %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

type apiClient struct {
	baseURL string
	tokenID string
	secret  string
	http    *http.Client
}

func (c *apiClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, out)
}

func (c *apiClient) form(ctx context.Context, method, path string, values url.Values, out any) error {
	return c.do(ctx, method, path, "application/x-www-form-urlencoded", strings.NewReader(values.Encode()), out)
}

func (c *apiClient) upload(ctx context.Context, path, filename string, content []byte, out any) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("content", "snippets"); err != nil {
		return fmt.Errorf("write upload content field: %w", err)
	}
	part, err := w.CreateFormFile("filename", filename)
	if err != nil {
		return fmt.Errorf("create upload part: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return fmt.Errorf("write upload part: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close upload body: %w", err)
	}
	return c.do(ctx, http.MethodPost, path, w.FormDataContentType(), &body, out)
}

func (c *apiClient) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build Proxmox request: %w", err)
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.tokenID+"="+c.secret)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Proxmox request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read Proxmox response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Body:       strings.TrimSpace(string(payload)),
		}
	}
	if out == nil {
		return nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode Proxmox response envelope: %w", err)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode Proxmox response data: %w", err)
	}
	return nil
}
