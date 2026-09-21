package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/janit/viiwork-parrot/internal/node"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func NewClient(listen string) *Client {
	return &Client{Base: "http://" + listen, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return 0, err
	}
	if method != http.MethodGet {
		// The daemon refuses mutating requests without it (browser
		// cross-origin hardening), even when there is no body.
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	trimmed := bytes.TrimSpace(data)
	if out != nil && (bytes.HasPrefix(trimmed, []byte("{")) || bytes.HasPrefix(trimmed, []byte("["))) {
		if err := json.Unmarshal(trimmed, out); err != nil {
			return resp.StatusCode, err
		}
	}
	if resp.StatusCode >= 400 && method != http.MethodPost {
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data))
	}
	return resp.StatusCode, nil
}

func (c *Client) Status(ctx context.Context) ([]node.ModelStatus, error) {
	var out []node.ModelStatus
	_, err := c.do(ctx, http.MethodGet, "/status", nil, &out)
	return out, err
}

// Ensure reports the HTTP status instead of failing on non-2xx, because 202,
// 404, 503 and 507 are all meaningful answers.
func (c *Client) Ensure(ctx context.Context, id string) (EnsureResponse, int, error) {
	var out EnsureResponse
	code, err := c.do(ctx, http.MethodPost, "/ensure", EnsureRequest{ID: id}, &out)
	return out, code, err
}

func (c *Client) Limits(ctx context.Context) (node.LimitsStatus, error) {
	var out node.LimitsStatus
	_, err := c.do(ctx, http.MethodGet, "/limits", nil, &out)
	return out, err
}

func (c *Client) SetOverride(ctx context.Context, o OverrideRequest) error {
	_, err := c.do(ctx, http.MethodPut, "/limits/override", o, nil)
	return err
}

func (c *Client) ClearOverride(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodDelete, "/limits/override", nil, nil)
	return err
}
