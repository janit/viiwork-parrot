package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/site"
)

func init() {
	register(command{"keygen", "create the ed25519 catalog signing key pair", runKeygen})
	register(command{"catalog", "catalog build|sign|verify|page", runCatalog})
}

func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	priv := fs.String("key", filepath.Join(os.Getenv("HOME"), ".config/viiwork-parrot/catalog.key"), "private key output (keep out of the repo)")
	pub := fs.String("pub", "keys/catalog.pub", "public key output (commit this)")
	fs.Parse(args)
	for _, p := range []string{*priv, *pub} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists; refusing to overwrite a signing key", p)
		}
	}
	pk, sk, err := catalog.GenerateKey()
	if err != nil {
		return err
	}
	for _, p := range []string{*priv, *pub} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(*priv, []byte(sk+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(*pub, []byte(pk+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("private key: %s\npublic key:  %s (%s)\n", *priv, *pub, pk)
	return nil
}

func runCatalog(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: viiwork-parrot catalog build|sign|verify|page [flags]")
	}
	fs := flag.NewFlagSet("catalog "+args[0], flag.ExitOnError)
	switch args[0] {
	case "build":
		in := fs.String("in", "catalog.yaml", "catalog source")
		out := fs.String("out", "dist/catalog.json", "output")
		fs.Parse(args[1:])
		models, err := catalog.LoadYAML(*in)
		if err != nil {
			return err
		}
		data, err := catalog.Build(models, time.Now())
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(*out, data, 0o644)
	case "sign":
		key := fs.String("key", filepath.Join(os.Getenv("HOME"), ".config/viiwork-parrot/catalog.key"), "private key")
		in := fs.String("in", "dist/catalog.json", "catalog to sign")
		fs.Parse(args[1:])
		sk, err := os.ReadFile(*key)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		sig, err := catalog.Sign(string(sk), data)
		if err != nil {
			return err
		}
		return os.WriteFile(*in+".sig", sig, 0o644)
	case "verify":
		pub := fs.String("pubkey", catalog.DefaultPubKey, "base64 public key (default: embedded)")
		in := fs.String("in", "dist/catalog.json", "catalog to verify")
		fs.Parse(args[1:])
		data, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(*in + ".sig")
		if err != nil {
			return err
		}
		if err := catalog.Verify(*pub, data, sig); err != nil {
			return err
		}
		if _, err := catalog.Parse(data); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	case "page":
		in := fs.String("in", "dist/catalog.json", "catalog to render")
		out := fs.String("out", "dist/index.html", "output HTML file")
		pubkeyFile := fs.String("pubkey-file", "keys/catalog.pub", "public key file, used when catalog.DefaultPubKey is not embedded")
		repoURL := fs.String("repo", "", "public repo URL shown in \"how to use\" (default https://github.com/janit/viiwork-parrot)")
		fs.Parse(args[1:])
		data, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		c, err := catalog.Parse(data)
		if err != nil {
			return err
		}
		opts := site.Options{RepoURL: *repoURL}
		if catalog.DefaultPubKey == "" {
			if pk, err := os.ReadFile(*pubkeyFile); err == nil {
				opts.PubKey = strings.TrimSpace(string(pk))
			}
		}
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		return site.Render(f, c, opts)
	}
	return fmt.Errorf("unknown catalog subcommand %q", args[0])
}
