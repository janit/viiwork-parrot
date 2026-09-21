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
