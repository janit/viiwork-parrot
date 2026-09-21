# 🦜 viiwork-parrot

The self-hosted BitTorrent tracker and seeding daemon viiwork uses to
distribute its model files. It serves the viiwork mesh first, but the swarm is
public: anyone can fetch from it. A companion to
[Pirate Face](https://pirateface.co/).
Related to viiwork only through the `/ensure` HTTP contract on its loopback
API — no shared code, independent releases. See `CHANGELOG.md` for what
v0.1.0 contains, and
`deploy/viiwork-parrot.yaml.example` and `deploy/tracker/README.md` for
running a node and a tracker respectively.

    make build        # bin/viiwork-parrot (embeds keys/catalog.pub)
    make test         # unit tests
    make itest        # integration tests (no network needed)

`viiwork-parrot catalog page --in dist/catalog.json --out dist/index.html`
renders a static landing page from the signed catalog (model list, magnet and
`.torrent` links, trackers, and the public key to verify `catalog.json`
against) — no JavaScript, generated only from the public catalog. See
`internal/site` for the template, and `scripts/catalog-publish.sh`, which
generates and uploads it alongside `catalog.json` on every publish.

## Seed-only nodes

With `models.seed_only_existing: true` a node never downloads: it adopts and
seeds only what is already on disk (data_dir, `models.adopt`,
`models.viiwork_configs`). On such a node `/ensure` answers `200` when the
model's weights are complete and verified; a documentation companion of a
GGUF model (README\*, LICENSE\*, NOTICE\*, \*.md, \*.txt) may be absent on
that node and is listed as `absent` under the model's files. Anything else
missing (a shard, a config file) leaves the model `absent` (`409`).

## Folder models

GGUF models are per-file: one single-file torrent per catalog file, stored
flat in `data_dir`. Hugging Face safetensors models (dozens to hundreds of
files, loaded by vLLM/SGLang as a directory) are published as **folder
models** instead (`layout: dir` in `catalog.yaml`): one multi-file torrent for
the whole model, stored as `data_dir/<model-id>/<hf path>` with the
repository's original names and subdirectories, and `/ensure` returns the
directory. Publish one from an `hf download --local-dir` copy:

    hf download <owner>/<repo> --revision <sha> --local-dir /data/<repo>
    viiwork-parrot mktorrent --layout dir --id <model-id> --repo <owner>/<repo> \
        --rev <sha> --license <id> [--license-url https://…] --local /data/<repo> --all

`--all` takes every file of the repo at that revision except
`.gitattributes` (or list files instead). Every file is size- and
hash-checked against Hugging Face before anything is written. A catalog
that contains a folder model is format version 2, which v0.1.0 nodes refuse
(they keep their cached catalog): upgrade every node before publishing one.
