package catalog

import (
	"errors"
	"testing"
)

func TestSignVerify(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1}`)
	sig, err := Sign(priv, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, data, sig); err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, []byte(`{"version":2}`), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body: %v", err)
	}
	bad := append([]byte(nil), sig...)
	bad[3] ^= 1
	if err := Verify(pub, data, bad); err == nil {
		t.Fatal("tampered sig verified")
	}
	pub2, _, _ := GenerateKey()
	if err := Verify(pub2, data, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	if err := Verify("", data, sig); !errors.Is(err, ErrNoPubKey) {
		t.Fatalf("no key: %v", err)
	}
}
