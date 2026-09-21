# Tracker deployment

This directory holds a self-hosted [opentracker](https://erdgeist.org/arts/software/opentracker/)
plus a static file server for `catalog.json`, `catalog.json.sig`, and
`torrents/*.torrent` — everything a `viiwork-parrot` node needs besides the
model data itself, which flows peer-to-peer.

`scripts/catalog-publish.sh` also uploads `index.html`, a static landing page
generated from the signed catalog (`viiwork-parrot catalog page`) for the
public site's `/`, which otherwise 404s. It's served by the same static file
server as `catalog.json` — no Caddy or opentracker configuration change is
needed; `index.html` just needs to land in `<PUBLIC_DIR>` like everything
else.

The templates here assume a common shape: the tracker host has no public IP
of its own and sits on a tailnet, fronted by a separately-run public-facing
reverse proxy (Cloudflare-proxied Caddy, in the reference deployment) that
forwards HTTP traffic over the tailnet. UDP announces bypass that chain
entirely, since UDP cannot be proxied through a CDN like Cloudflare.

```
Public front end (e.g. Cloudflare-proxied Caddy)
        │  https://<your-tracker-hostname>/
        ▼
Tailnet
        │  <TAILNET_IP>:8080
        ▼
Tracker host: Caddy :8080 (no TLS) -> opentracker (127.0.0.1:6969 TCP) / static files
```

UDP announces (`udp://<your-udp-host>:6969/announce`) go **directly** to
whatever forwards UDP 6969 to the tracker host — typically a home router's
port forward to the tracker host's LAN address, since UDP cannot ride the
front end above.

The tracker host itself needs no TLS and no certificates for this: Caddy on
`:8080` serves plain HTTP over the tailnet, and TLS termination (if any) is
the front end's job.

## Trust boundary: CF-Connecting-IP

A CDN/reverse-proxy front end that terminates TLS for the public hostname
(Cloudflare, in the reference deployment) sets a header carrying the real
internet client's IP on every request it proxies (`CF-Connecting-IP`, in
Cloudflare's case), and the front end's own Caddy passes it through
unmodified to the tracker host. `Caddyfile.example` uses that header to set
`X-Forwarded-For` for `/announce`, which is what opentracker
(`WANT_IP_FROM_PROXY` + `access.proxy 127.0.0.1`) uses as the peer's real IP
instead of the tailnet address it would otherwise see.

That header is only trustworthy when the request actually came through that
chain. The tracker's Caddy listens on the tailnet address
(`<TAILNET_IP>:8080`), which every tailnet host can reach directly, not only
the front end — so without a check, any tailnet host could hit `/announce`
directly with a forged CF-Connecting-IP and make opentracker record an
arbitrary IP as a peer's address (poisoning the swarm for other clients).

`Caddyfile.example` therefore only honours that header for requests whose
**immediate** remote address is the front end's tailnet IP
(`<FRONTEND_TAILNET_IP>`, the `@frontend_cf` matcher, which also requires the
header to look like an IP address). A request from the front end *without*
a usable CF-Connecting-IP falls back to the first hop of the
`X-Forwarded-For` the front end's Caddy set (its view of the connecting
client), and failing that to the front end's own address —
`X-Forwarded-For` is never sent empty. Every other request to `/announce` —
including one from another tailnet host, or one that reaches Caddy over
`127.0.0.1` — gets `X-Forwarded-For` set (overwritten, not appended) to the
real connecting address instead, so a forged CF-Connecting-IP or
X-Forwarded-For from an untrusted source is never passed through to
opentracker.

That still trusts the front end to only pass on a CF-Connecting-IP that
Cloudflare (or your CDN) actually set. If the front end host is reachable
directly, bypassing the CDN, anyone could send the public Host header with
their own CF-Connecting-IP straight to it. The front-end Caddy block below
therefore **aborts every request that does not come from a Cloudflare IP
range** (`@notcf not remote_ip …` → `abort`) before it ever reaches the
tracker. Keep the front end's Caddy without `trusted_proxies` for this site,
so the `X-Forwarded-For` it sends is its own view of the connecting address,
not a client-supplied value.

`Caddyfile.example`'s site address is host-less (`:8080`, with `bind
<TAILNET_IP> 127.0.0.1`): the front end forwards the original public Host
header, and a site address naming an IP would act as a host matcher instead,
so those requests would match no site and get Caddy's empty `200`.

Append a block like this to the front end's own Caddyfile, replacing
`<TRACKER_TAILNET_IP>` with the tracker host's tailnet address and the site
address with your public hostname. Requests that don't come from Cloudflare
are aborted (connection closed, no response) — see above for why. The
ranges are Cloudflare's published lists; re-check them occasionally
(Cloudflare announces changes in advance) at
<https://www.cloudflare.com/ips-v4> and <https://www.cloudflare.com/ips-v6>.

```
your-tracker-hostname.example.com {
  # viiwork-parrot tracker, reached over the tailnet.
  # Only Cloudflare may connect: the tracker host trusts the
  # CF-Connecting-IP header passed through here (it becomes the peer's IP in
  # the swarm), and this front end may be reachable directly, bypassing
  # Cloudflare.
  @notcf not remote_ip 173.245.48.0/20 103.21.244.0/22 103.22.200.0/22 103.31.4.0/22 141.101.64.0/18 108.162.192.0/18 190.93.240.0/20 188.114.96.0/20 197.234.240.0/22 198.41.128.0/17 162.158.0.0/15 104.16.0.0/13 104.24.0.0/14 172.64.0.0/13 131.0.72.0/22 2400:cb00::/32 2606:4700::/32 2803:f800::/32 2405:b500::/32 2405:8100::/32 2a06:98c0::/29 2c0f:f248::/32
  abort @notcf
  header /announce* Cache-Control "no-store"
  header /scrape* Cache-Control "no-store"
  header /catalog.json* Cache-Control "public, max-age=60"
  header /torrents/* Cache-Control "public, max-age=86400, immutable"
  reverse_proxy <TRACKER_TAILNET_IP>:8080
}
```

## Setup

1. Copy `Caddyfile.example` to `Caddyfile` and `docker-compose.example.yaml`
   to `docker-compose.yaml` (both git-ignored), and fill in `<TAILNET_IP>`,
   `<FRONTEND_TAILNET_IP>` and `<PUBLIC_DIR>`.
2. Add the front-end Caddy block above to your public-facing reverse proxy,
   with its own `<TRACKER_TAILNET_IP>` placeholder filled in, and point your
   DNS at it (proxied, if using Cloudflare).
3. Forward UDP 6969 from whatever host has the public IP straight to the
   tracker host, for direct UDP announces.
4. `docker compose up -d --build` on the tracker host.

## Verifying

```bash
python3 scripts/udp-announce-check.py <TRACKER_TAILNET_IP> 6969   # tailnet: udp tracker ok
python3 scripts/udp-announce-check.py <your-udp-host> 6969        # public via router forward
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: <your-tracker-hostname>' http://<TRACKER_TAILNET_IP>:8080/catalog.json
curl -s -o /dev/null -w '%{http_code}\n' https://<your-tracker-hostname>/catalog.json
```

## Image pinning

`Dockerfile` (alpine) and `docker-compose.example.yaml` (caddy) pin their
base images by multi-arch index digest. To update, resolve the tag again
(`docker buildx imagetools inspect alpine:3.22`,
`... caddy:2-alpine`), replace the digests, and redeploy with `--build`.

## This deployment's concrete configuration

The reference deployment (real hostnames, tailnet IPs, and the exact deploy
command) lives in a host-specific runbook under `docs/private/`, which is excluded
from the public release.
