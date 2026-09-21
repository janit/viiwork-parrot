// Package config loads viiwork-parrot.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/janit/viiwork-parrot/internal/schedule"
	"github.com/janit/viiwork-parrot/internal/units"
)

type Config struct {
	Node    Node    `yaml:"node"`
	Catalog Catalog `yaml:"catalog"`
	Models  Models  `yaml:"models"`
	Network Network `yaml:"network"`
	Local   Local   `yaml:"local"`
	Limits  Limits  `yaml:"limits"`
	API     API     `yaml:"api"`

	// Schedule is compiled from Limits by Parse.
	Schedule schedule.Schedule `yaml:"-"`
}

type Node struct {
	Name     string `yaml:"name"`
	DataDir  string `yaml:"data_dir"`
	StateDir string `yaml:"state_dir"`
}

type Catalog struct {
	URL     string        `yaml:"url"`
	Refresh time.Duration `yaml:"refresh"`
	PubKey  string        `yaml:"pubkey"`
}

type Models struct {
	Want             []string `yaml:"want"`
	SeedOnlyExisting bool     `yaml:"seed_only_existing"`
	Prune            bool     `yaml:"prune"`
	Adopt            []string `yaml:"adopt"`
	// ViiworkConfigs are viiwork.yaml files whose model paths are adoption
	// candidates, so a host running both never stores a model twice.
	ViiworkConfigs []string `yaml:"viiwork_configs"`
}

type Network struct {
	ListenPort int  `yaml:"listen_port"`
	UPnP       bool `yaml:"upnp"`
	DHT        bool `yaml:"dht"`
	PEX        bool `yaml:"pex"`
}

type Local struct {
	CIDRs           []string `yaml:"cidrs"`
	Throttle        bool     `yaml:"throttle"`
	CountInMaxConns bool     `yaml:"count_in_max_conns"`
	LSD             bool     `yaml:"lsd"`
	Tailscale       bool     `yaml:"tailscale"`
	Peers           []string `yaml:"peers"`
}

type RawLimits struct {
	Upload             *string `yaml:"upload"`
	Download           *string `yaml:"download"`
	MaxConns           *int    `yaml:"max_conns"`
	MaxConnsPerTorrent *int    `yaml:"max_conns_per_torrent"`
	MaxHalfOpen        *int    `yaml:"max_half_open"`
	MaxActiveUploads   *int    `yaml:"max_active_uploads"`
}

type Limits struct {
	RawLimits `yaml:",inline"`
	Schedule  []Rule `yaml:"schedule"`
}

type Rule struct {
	Days      []string `yaml:"days"`
	From      string   `yaml:"from"`
	To        string   `yaml:"to"`
	RawLimits `yaml:",inline"`
}

type API struct {
	Listen string `yaml:"listen"`
}

// DefaultLocalCIDRs: RFC1918, loopback, the Tailscale v4/v6 ranges, and
// IPv6 link-local and unique-local (ULA) addresses.
var DefaultLocalCIDRs = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8",
	"100.64.0.0/10", "fd7a:115c:a1e0::/48", "::1/128",
	"fe80::/10", "fc00::/7",
}

var defaultLimits = schedule.Limits{MaxConns: 200, MaxConnsPerTorrent: 50, MaxHalfOpen: 25, MaxActiveUploads: 4}

func Defaults() Config {
	return Config{
		Node:    Node{DataDir: "/models", StateDir: "/var/lib/viiwork-parrot"},
		Catalog: Catalog{URL: "https://parrot.lnx.fi/catalog.json", Refresh: time.Hour},
		Network: Network{ListenPort: 42069, UPnP: true, DHT: true, PEX: true},
		Local:   Local{CIDRs: append([]string(nil), DefaultLocalCIDRs...), LSD: true, Tailscale: true},
		API:     API{Listen: "127.0.0.1:7950"},
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	c := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Node.DataDir == "" {
		return errors.New("node.data_dir: required")
	}
	if c.Node.StateDir == "" {
		return errors.New("node.state_dir: required")
	}
	if u, err := url.Parse(c.Catalog.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("catalog.url: %q is not an http(s) URL", c.Catalog.URL)
	}
	if c.Catalog.Refresh <= 0 {
		return errors.New("catalog.refresh: must be positive")
	}
	// 0 means "pick a random port" (used by tests); anything else must be a valid port number.
	if c.Network.ListenPort < 0 || c.Network.ListenPort > 65535 {
		return fmt.Errorf("network.listen_port: %d out of range", c.Network.ListenPort)
	}
	for i, s := range c.Local.CIDRs {
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("local.cidrs[%d]: %v", i, err)
		}
	}
	host, _, err := net.SplitHostPort(c.API.Listen)
	if err != nil {
		return fmt.Errorf("api.listen: %v", err)
	}
	if ip, err := netip.ParseAddr(host); host != "localhost" && (err != nil || !ip.IsLoopback()) {
		return fmt.Errorf("api.listen: %q must be a loopback address (the API has no auth)", c.API.Listen)
	}
	return c.compileSchedule()
}

func (c *Config) compileSchedule() error {
	base := defaultLimits
	p, err := toPartial("limits", c.Limits.RawLimits)
	if err != nil {
		return err
	}
	base = p.Apply(base)
	if c.Limits.MaxHalfOpen != nil {
		if *c.Limits.MaxHalfOpen < 0 {
			return errors.New("limits.max_half_open: must be >= 0")
		}
		base.MaxHalfOpen = *c.Limits.MaxHalfOpen
	}
	s := schedule.Schedule{Base: base}
	for i, r := range c.Limits.Schedule {
		key := fmt.Sprintf("limits.schedule[%d]", i)
		if r.MaxHalfOpen != nil {
			return fmt.Errorf("%s.max_half_open: only allowed in base limits (applied at startup)", key)
		}
		days, err := schedule.ParseDays(r.Days)
		if err != nil {
			return fmt.Errorf("%s.days: %v", key, err)
		}
		from, to := 0, 0
		if r.From != "" {
			if from, err = schedule.ParseClock(r.From); err != nil {
				return fmt.Errorf("%s.from: %v", key, err)
			}
		}
		if r.To != "" {
			if to, err = schedule.ParseClock(r.To); err != nil {
				return fmt.Errorf("%s.to: %v", key, err)
			}
		}
		set, err := toPartial(key, r.RawLimits)
		if err != nil {
			return err
		}
		s.Rules = append(s.Rules, schedule.Rule{Days: days, From: from, To: to, Set: set})
	}
	c.Schedule = s
	return nil
}

func toPartial(key string, r RawLimits) (schedule.Partial, error) {
	var p schedule.Partial
	rate := func(name string, s *string) (*int64, error) {
		if s == nil {
			return nil, nil
		}
		v, err := units.ParseRate(*s)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %v", key, name, err)
		}
		return &v, nil
	}
	var err error
	if p.Upload, err = rate("upload", r.Upload); err != nil {
		return p, err
	}
	if p.Download, err = rate("download", r.Download); err != nil {
		return p, err
	}
	for name, v := range map[string]*int{"max_conns": r.MaxConns, "max_conns_per_torrent": r.MaxConnsPerTorrent, "max_active_uploads": r.MaxActiveUploads} {
		if v != nil && *v < 0 {
			return p, fmt.Errorf("%s.%s: must be >= 0", key, name)
		}
	}
	p.MaxConns, p.MaxConnsPerTorrent, p.MaxActiveUploads = r.MaxConns, r.MaxConnsPerTorrent, r.MaxActiveUploads
	return p, nil
}
