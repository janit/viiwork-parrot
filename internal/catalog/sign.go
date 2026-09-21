package catalog

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// DefaultPubKey is embedded at build time from keys/catalog.pub (see Makefile).
var DefaultPubKey = ""

var (
	ErrBadSignature = errors.New("catalog signature does not verify")
	ErrNoPubKey     = errors.New("no catalog public key configured")
)

func GenerateKey() (pub, priv string, err error) {
	p, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(p), base64.StdEncoding.EncodeToString(k), nil
}

func Sign(privB64 string, data []byte) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privB64))
	if err != nil || len(k) != ed25519.PrivateKeySize {
		return nil, errors.New("bad private key")
	}
	sig := ed25519.Sign(ed25519.PrivateKey(k), data)
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n"), nil
}

func Verify(pubB64 string, data, sig []byte) error {
	if strings.TrimSpace(pubB64) == "" {
		return ErrNoPubKey
	}
	p, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubB64))
	if err != nil || len(p) != ed25519.PublicKeySize {
		return fmt.Errorf("bad public key")
	}
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return fmt.Errorf("%w: malformed signature", ErrBadSignature)
	}
	if !ed25519.Verify(ed25519.PublicKey(p), data, s) {
		return ErrBadSignature
	}
	return nil
}
