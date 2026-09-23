package config

import (
	"strings"
	"testing"
	"time"
)

const example = `
node:
  name: node1
  data_dir: /models
  state_dir: /var/lib/viiwork-parrot
catalog:
  url: https://parrot.lnx.fi/catalog.json
  refresh: 30m
models:
  want: [qwen3.6-27b-q4km]
  adopt: [/home/user/models]
  viiwork_configs: [/home/user/viiwork/viiwork.yaml]
network:
  listen_port: 42069
  upnp: false
local:
  peers: ["node-a.lan:42069"]
limits:
  upload: 50Mbit
  max_conns: 200
  schedule:
    - days: [mon-fri]
      from: "08:00"
      to: "18:00"
      upload: 10Mbit
      max_conns: 60
    - from: "00:00"
      to: "06:00"
      upload: 0
api:
  listen: 127.0.0.1:7950
`

func TestParseExample(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models.ViiworkConfigs) != 1 || c.Models.ViiworkConfigs[0] != "/home/user/viiwork/viiwork.yaml" {
		t.Fatalf("viiwork_configs: %v", c.Models.ViiworkConfigs)
	}
	if c.Catalog.Refresh != 30*time.Minute || c.Network.UPnP || !c.Network.DHT || !c.Network.PEX {
		t.Fatalf("network/catalog: %+v %+v", c.Catalog, c.Network)
	}
	if len(c.Local.CIDRs) != len(DefaultLocalCIDRs) || !c.Local.LSD || !c.Local.Tailscale {
		t.Fatalf("local defaults lost: %+v", c.Local)
	}
	b := c.Schedule.Base
	if b.Upload != 6_250_000 || b.Download != 0 || b.MaxConns != 200 || b.MaxConnsPerTorrent != 50 || b.MaxHalfOpen != 25 || b.MaxActiveUploads != 4 {
		t.Fatalf("base: %+v", b)
	}
	if len(c.Schedule.Rules) != 2 || *c.Schedule.Rules[0].Set.Upload != 1_250_000 || *c.Schedule.Rules[0].Set.MaxConns != 60 || *c.Schedule.Rules[1].Set.Upload != 0 {
		t.Fatalf("rules: %+v", c.Schedule.Rules)
	}
	if c.Schedule.Rules[0].From != 480 || c.Schedule.Rules[0].Days[0] {
		t.Fatalf("rule 0 window: %+v", c.Schedule.Rules[0])
	}
}

func TestEmptyConfigIsDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Node.DataDir != "/models" || c.API.Listen != "127.0.0.1:7950" || c.Network.ListenPort != 42069 {
		t.Fatalf("%+v", c)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"limits:\n  upload: 10 parsecs\n":                                "limits.upload",
		"limits:\n  max_half_open: -5\n":                                 "limits.max_half_open",
		"limits:\n  schedule:\n    - from: \"08:00\"\n      to: \"9\"\n": "limits.schedule[0].to",
		"limits:\n  schedule:\n    - days: [funday]\n":                   "limits.schedule[0].days",
		"limits:\n  schedule:\n    - upload: 1Gbit2\n":                   "limits.schedule[0].upload",
		"limits:\n  schedule:\n    - max_half_open: 5\n":                 "limits.schedule[0].max_half_open",
		"api:\n  listen: 0.0.0.0:7950\n":                                 "api.listen",
		"local:\n  cidrs: [10.0.0.0/33]\n":                               "local.cidrs[0]",
		"network:\n  listen_port: 70000\n":                               "network.listen_port",
		"catalog:\n  url: ftp://x\n":                                     "catalog.url",
		"nodez:\n  name: x\n":                                            "nodez",
		"node:\n  data_dir: \"\"\n":                                      "node.data_dir",
		"node:\n  state_dir: \"\"\n":                                     "node.state_dir",
		"catalog:\n  refresh: 0s\n":                                      "catalog.refresh",
		"limits:\n  schedule:\n    - from: \"25:00\"\n":                  "limits.schedule[0].from",
		"limits:\n  max_conns: -1\n":                                     "limits.max_conns",
		"api:\n  listen: 127.0.0.1\n":                                    "api.listen",
	}
	for in, key := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("config %q: error %v should name %q", in, err, key)
		}
	}
}

// TestDefaultLocalCIDRsCoverIPv6LAN: IPv6 link-local and ULA peers on the
// same LAN are local by default, like their RFC1918 counterparts.
func TestDefaultLocalCIDRsCoverIPv6LAN(t *testing.T) {
	have := map[string]bool{}
	for _, c := range Defaults().Local.CIDRs {
		have[c] = true
	}
	for _, want := range []string{"fe80::/10", "fc00::/7", "10.0.0.0/8", "100.64.0.0/10"} {
		if !have[want] {
			t.Errorf("default local.cidrs missing %s", want)
		}
	}
}
