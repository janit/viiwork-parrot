package node

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestViiworkModelPaths(t *testing.T) {
	dir := t.TempDir()
	v1 := filepath.Join(dir, "v1.yaml")
	os.WriteFile(v1, []byte("server:\n  port: 8080\nmodel:\n  path: /models/a.gguf\n"), 0o644)
	v2 := filepath.Join(dir, "v2.yaml")
	os.WriteFile(v2, []byte("node:\n  name: x\nmodels:\n  - name: b\n    path: /m/b.gguf\n  - name: c\n    path: /m/c-00001-of-00002.gguf\n"), 0o644)
	if got, err := viiworkModelPaths(v1); err != nil || !reflect.DeepEqual(got, []string{"/models/a.gguf"}) {
		t.Fatalf("v1: %v %v", got, err)
	}
	if got, err := viiworkModelPaths(v2); err != nil || !reflect.DeepEqual(got, []string{"/m/b.gguf", "/m/c-00001-of-00002.gguf"}) {
		t.Fatalf("v2: %v %v", got, err)
	}
	if _, err := viiworkModelPaths(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file must error")
	}
}
