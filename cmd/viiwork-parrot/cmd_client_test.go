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

func TestWriteStatusZeroRateIsNotUnlimited(t *testing.T) {
	st := []node.ModelStatus{
		{ID: "idle", State: node.StateSeeding, Path: "/models/idle.gguf"},
		{ID: "busy", State: node.StateSeeding, Path: "/models/busy.gguf", UpRate: 2_500_000},
	}
	var buf bytes.Buffer
	if err := writeStatus(&buf, st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "unlimited") {
		t.Fatalf("status must not show a zero rate as unlimited:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 6 {
			continue
		}
		switch f[0] {
		case "idle":
			if f[3] != "0" || f[4] != "0" {
				t.Fatalf("idle rates = %q/%q, want 0/0", f[3], f[4])
			}
		case "busy":
			if f[3] != "0" || f[4] != "20.0Mbit" {
				t.Fatalf("busy rates = %q/%q, want 0/20.0Mbit", f[3], f[4])
			}
		}
	}
}

func TestFormatObservedRate(t *testing.T) {
	for in, want := range map[int64]string{0: "0", -1: "0", 125: "1Kbit", 125_000_000: "1.0Gbit"} {
		if got := formatObservedRate(in); got != want {
			t.Errorf("formatObservedRate(%d) = %q, want %q", in, got, want)
		}
	}
}
