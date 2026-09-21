package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/hfapi"
	"github.com/janit/viiwork-parrot/internal/publish"
)

func init() {
	register(command{"mktorrent", "add a model to catalog.yaml from local copies of its HF files", runMktorrent})
}

type kvFlag map[string]string

func (k kvFlag) String() string { return fmt.Sprint(map[string]string(k)) }
func (k kvFlag) Set(s string) error {
	a, b, ok := strings.Cut(s, "=")
	if !ok {
		return errors.New("want hfpath=diskname")
	}
	k[a] = b
	return nil
}

func runMktorrent(args []string) error {
	fs := flag.NewFlagSet("mktorrent", flag.ExitOnError)
	cat := fs.String("catalog", "catalog.yaml", "catalog source to update")
	torrents := fs.String("torrents", "torrents", "torrent output dir")
	id := fs.String("id", "", "model id, e.g. gemma4-31b-qat-q4kxl")
	repo := fs.String("repo", "", "HF repo, e.g. unsloth/gemma-4-31B-it-qat-GGUF")
	rev := fs.String("rev", "main", "HF revision (branch or sha; stored as sha)")
	license := fs.String("license", "", "license id, e.g. apache-2.0, gemma")
	local := fs.String("local", ".", "dir holding the files at their HF paths")
	replace := fs.Bool("replace", false, "replace an existing entry with the same id")
	storeAs := kvFlag{}
	fs.Var(storeAs, "store-as", "hfpath=diskname (repeatable)")
	fs.Parse(args)
	if *id == "" || *repo == "" || *license == "" || fs.NArg() == 0 {
		return errors.New("usage: viiwork-parrot mktorrent --id ID --repo OWNER/NAME --license L [--rev REV] [--local DIR] [--store-as p=n] FILE...")
	}
	models, err := catalog.LoadYAML(*cat)
	if err != nil {
		return err
	}
	idx := -1
	for i, m := range models {
		if m.ID == *id {
			idx = i
		}
	}
	if idx >= 0 && !*replace {
		return fmt.Errorf("model %s already in %s (use --replace)", *id, *cat)
	}
	m, err := publish.AddModel(context.Background(), publish.Options{
		ID: *id, Repo: *repo, Revision: *rev, License: *license, LocalDir: *local,
		Files: fs.Args(), StoreAs: storeAs, TorrentsDir: *torrents, HF: hfapi.New(),
	})
	if err != nil {
		return err
	}
	if idx >= 0 {
		models[idx] = m
	} else {
		models = append(models, m)
	}
	if err := catalog.Validate(models); err != nil {
		return err
	}
	if err := catalog.SaveYAML(*cat, models); err != nil {
		return err
	}
	for _, f := range m.Files {
		fmt.Printf("%s  %s  %s\n", f.InfoHash, f.DiskName(), f.Name)
	}
	return nil
}
