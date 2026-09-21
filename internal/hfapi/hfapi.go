// Package hfapi is the small slice of the Hugging Face Hub API viiwork-parrot uses.
package hfapi

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New() *Client {
	return &Client{BaseURL: "https://huggingface.co", Token: os.Getenv("HF_TOKEN"), HTTP: &http.Client{Timeout: 60 * time.Second}}
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c *Client) ResolveRevision(ctx context.Context, repo, rev string) (string, error) {
	if shaRe.MatchString(rev) {
		return rev, nil
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if _, err := c.getJSON(ctx, c.BaseURL+"/api/models/"+repo+"/revision/"+url.PathEscape(rev), &out); err != nil {
		return "", err
	}
	if !shaRe.MatchString(out.SHA) {
		return "", fmt.Errorf("revision %s@%s: API returned sha %q", repo, rev, out.SHA)
	}
	return out.SHA, nil
}

type LFS struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type Entry struct {
	Type string `json:"type"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	LFS  *LFS   `json:"lfs"`
}

func (c *Client) Tree(ctx context.Context, repo, sha string) ([]Entry, error) {
	var all []Entry
	next := c.BaseURL + "/api/models/" + repo + "/tree/" + sha + "?recursive=true"
	for next != "" {
		var page []Entry
		h, err := c.getJSON(ctx, next, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = nextLink(h.Get("Link"))
	}
	return all, nil
}

var linkRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(h string) string {
	if m := linkRe.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

func (c *Client) getJSON(ctx context.Context, u string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

// ResolveURL is the public download URL used as the torrent web-seed. It is
// always the real HF host, never BaseURL.
func ResolveURL(repo, sha, path string) string {
	return "https://huggingface.co/" + repo + "/resolve/" + sha + "/" + path
}

// GitBlobSHA1 computes git's object id for a non-LFS file.
func GitBlobSHA1(r io.Reader, size int64) (string, error) {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", size)
	n, err := io.Copy(h, r)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("read %d bytes, expected %d", n, size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
