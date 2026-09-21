package hfapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var sha = strings.Repeat("ab", 20)

func TestResolveRevisionAndTree(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing token")
		}
		switch r.URL.Path {
		case "/api/models/o/r/revision/main":
			fmt.Fprintf(w, `{"sha":%q}`, sha)
		case "/api/models/o/r/tree/" + sha:
			if r.URL.Query().Get("cursor") == "" {
				w.Header().Set("Link", fmt.Sprintf(`<%s/api/models/o/r/tree/%s?recursive=true&cursor=2>; rel="next"`, srv.URL, sha))
				fmt.Fprint(w, `[{"type":"file","oid":"ce013625030ba8dba906f756967f9e9ca394464a","size":6,"path":"LICENSE"}]`)
				return
			}
			fmt.Fprint(w, `[{"type":"file","oid":"x","size":100,"path":"q/m.gguf","lfs":{"oid":"`+strings.Repeat("c", 64)+`","size":100}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}

	got, err := c.ResolveRevision(context.Background(), "o/r", "main")
	if err != nil || got != sha {
		t.Fatalf("%s %v", got, err)
	}
	if got, _ := c.ResolveRevision(context.Background(), "o/r", sha); got != sha {
		t.Fatal("a full sha resolves to itself without a request")
	}
	entries, err := c.Tree(context.Background(), "o/r", sha)
	if err != nil || len(entries) != 2 || entries[1].LFS == nil || entries[1].Path != "q/m.gguf" {
		t.Fatalf("%+v %v", entries, err)
	}
}

func TestGitBlobSHA1(t *testing.T) {
	// `printf 'hello\n' | git hash-object --stdin`
	got, err := GitBlobSHA1(strings.NewReader("hello\n"), 6)
	if err != nil || got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := GitBlobSHA1(strings.NewReader("hello\n"), 7); err == nil {
		t.Fatal("size mismatch must error")
	}
}

func TestResolveURL(t *testing.T) {
	if got := ResolveURL("o/r", sha, "q/m.gguf"); got != "https://huggingface.co/o/r/resolve/"+sha+"/q/m.gguf" {
		t.Fatal(got)
	}
}
