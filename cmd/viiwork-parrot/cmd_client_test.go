package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/janit/viiwork-parrot/internal/node"
)

func TestWriteStatusDuplicates(t *testing.T) {
	st := []node.ModelStatus{
		{ID: "gemma", State: node.StateSeeding, Path: "/models/gemma.gguf", Duplicates: []string{"/home/user/gemma-copy.gguf", "/mnt/backup/gemma.gguf"}},
		{ID: "phi", State: node.StateDownloading, Percent: 12.5},
	}
	var buf bytes.Buffer
	if err := writeStatus(&buf, st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	dupIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "gemma") && !strings.Contains(l, "duplicates") {
			dupIdx = i
		}
	}
	if dupIdx == -1 || dupIdx+1 >= len(lines) {
		t.Fatalf("expected a duplicates line right after gemma's row; got:\n%s", out)
	}
	dupLine := lines[dupIdx+1]
	if !strings.Contains(dupLine, "duplicates:") {
		t.Fatalf("expected an indented duplicates line after gemma, got %q", dupLine)
	}
	if !strings.Contains(dupLine, "/home/user/gemma-copy.gguf") || !strings.Contains(dupLine, "/mnt/backup/gemma.gguf") {
		t.Fatalf("duplicates line missing paths: %q", dupLine)
	}
	if !strings.HasPrefix(dupLine, "  ") {
		t.Fatalf("duplicates line should be indented, got %q", dupLine)
	}
	if strings.Contains(out, "phi") && strings.Count(out, "duplicates:") != 1 {
		t.Fatalf("only the model with duplicates should get a duplicates line:\n%s", out)
	}
}
