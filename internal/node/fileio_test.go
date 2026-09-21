package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const sigbusChildEnv = "VIIWORK_PARROT_SIGBUS_CHILD"

// TestTruncatedAdoptedFileDoesNotCrash: an adopted file is someone else's
// and may be truncated under us while we seed it. With anacrolix's mmap
// file IO, reading it then kills the whole process with SIGBUS; with
// classic IO it is only a read error. EnsureClassicFileIO (called first
// thing by the daemon) must select classic IO. anacrolix reads its knob at
// package init, so this runs in a child process whose environment lacks it,
// exactly like a daemon started by hand.
func TestTruncatedAdoptedFileDoesNotCrash(t *testing.T) {
	if os.Getenv(sigbusChildEnv) == "1" {
		if err := EnsureClassicFileIO(); err != nil {
			t.Fatal(err)
		}
		readTruncatedAdoptedFile(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTruncatedAdoptedFileDoesNotCrash$", "-test.count=1")
	cmd.Env = append(withoutEnv(os.Environ(), FileIOEnv), sigbusChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child crashed or failed: %v\n%s", err, head(out, 1500))
	}
}

func readTruncatedAdoptedFile(t *testing.T) {
	fx := newFixture(t)
	src, _ := fx.addModel("tiny", "tiny.gguf", 4<<20)
	adoptDir := t.TempDir()
	data, _ := os.ReadFile(src)
	p := filepath.Join(adoptDir, "mine.gguf")
	os.WriteFile(p, data, 0o644)
	n := fx.node(func(n *Node) { n.cfg.Models.Want = []string{"tiny"}; n.cfg.Models.Adopt = []string{adoptDir} })
	n.SetCatalog(fx.cat)
	waitState(t, n, "tiny", StateSeeding)
	if err := os.Truncate(p, 0); err != nil { // the owner shrinks their file
		t.Fatal(err)
	}
	n.mu.Lock()
	j := n.jobs[fx.cat.Models[0].Files[0].InfoHash]
	n.mu.Unlock()
	r := j.torrent().NewReader()
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.ReadContext(ctx, make([]byte, 1<<20)) // an error or a timeout is fine; a SIGBUS is not
}

func withoutEnv(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if len(kv) > len(key) && kv[:len(key)+1] == key+"=" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func head(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}
