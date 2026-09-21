package discovery

import (
	"context"
	"net"
	"reflect"
	"testing"
)

const status = `{
  "Self": {"HostName": "node1", "TailscaleIPs": ["100.1.1.1"], "Online": true},
  "Peer": {
    "nodekey:a": {"HostName": "node-a", "TailscaleIPs": ["100.64.0.21", "fd7a:115c:a1e0::1"], "Online": true},
    "nodekey:b": {"HostName": "node-b", "TailscaleIPs": ["100.64.0.22"], "Online": false}
  }
}`

func TestParseTailscaleStatus(t *testing.T) {
	got, err := ParseTailscaleStatus([]byte(status), 42069)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"100.64.0.21:42069", "[fd7a:115c:a1e0::1]:42069"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, err := ParseTailscaleStatus([]byte("not json"), 1); err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveStatic(t *testing.T) {
	got, errs := ResolveStatic(context.Background(), net.DefaultResolver, []string{"127.0.0.1:1", "localhost:2", "no-port", "nonexistent.invalid:3"})
	if len(errs) != 2 {
		t.Fatalf("errs: %v", errs)
	}
	has := map[string]bool{}
	for _, g := range got {
		has[g] = true
	}
	if !has["127.0.0.1:1"] || !(has["127.0.0.1:2"] || has["[::1]:2"]) {
		t.Fatalf("got %v", got)
	}
}
