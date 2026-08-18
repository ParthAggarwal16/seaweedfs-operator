package seaweed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrNotFound = errors.New("seaweedfs: path not found")

type FilerClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewFilerClient(baseURL string) *FilerClient {
	return &FilerClient{
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *FilerClient) ReadFile(ctx context.Context, path string) ([]byte, error) {
	u := c.BaseURL + ensureLeadingSlash(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build filer read request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("filer read %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("filer read %s body: %w", path, err)
		}
		return body, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("filer read %s: unexpected status %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
}

func (c *FilerClient) WriteFile(ctx context.Context, path string, content []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", lastPathSegment(path))
	if err != nil {
		return fmt.Errorf("build filer upload form: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return fmt.Errorf("write filer upload form: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close filer upload form: %w", err)
	}

	u := c.BaseURL + ensureLeadingSlash(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return fmt.Errorf("build filer write request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("filer write %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("filer write %s: unexpected status %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *FilerClient) DeletePath(ctx context.Context, path string, recursive bool) error {
	u := c.BaseURL + ensureLeadingSlash(path)
	if recursive {
		u += "?recursive=true&ignoreRecursiveError=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("build filer delete request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("filer delete %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("filer delete %s: unexpected status %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *FilerClient) Healthz(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("filer healthz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("filer healthz: status %s", resp.Status)
	}
	return nil
}

func (c *FilerClient) SetBucketQuota(ctx context.Context, bucketsRoot, bucket string, sizeBytes int64, enforce bool) error {
	if !enforce {
		sizeBytes = -sizeBytes
	}
	target := fmt.Sprintf("%s/%s?tagging&collection.quota=%d", c.BaseURL+ensureLeadingSlash(bucketsRoot), url.PathEscape(bucket), sizeBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, nil)
	if err != nil {
		return fmt.Errorf("build quota request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("set bucket quota %s: %w", bucket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("set bucket quota %s: status %s: %s", bucket, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func ensureLeadingSlash(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

func lastPathSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}
