// Package discovery finds local peers that trackers cannot tell us about:
// tailnet hosts and statically configured LAN hosts. LAN multicast (BEP-14)
// is handled inside anacrolix.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"sort"
	"strconv"
)

type tsPeer struct {
	HostName     string   `json:"HostName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
}

type tsStatus struct {
	Peer map[string]tsPeer `json:"Peer"`
}

func ParseTailscaleStatus(data []byte, port int) ([]string, error) {
	var s tsStatus
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var out []string
	for _, p := range s.Peer {
		if !p.Online {
			continue
		}
		for _, ip := range p.TailscaleIPs {
			a, err := netip.ParseAddr(ip)
			if err != nil {
				continue
			}
			out = append(out, netip.AddrPortFrom(a, uint16(port)).String())
		}
	}
	sort.Strings(out)
	return out, nil
}

func TailscalePeers(ctx context.Context, port int) ([]string, error) {
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	return ParseTailscaleStatus(out, port)
}

func ResolveStatic(ctx context.Context, r *net.Resolver, peers []string) ([]string, []error) {
	var out []string
	var errs []error
	for _, p := range peers {
		host, portStr, err := net.SplitHostPort(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("local.peers %q: %w", p, err))
			continue
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			errs = append(errs, fmt.Errorf("local.peers %q: bad port", p))
			continue
		}
		if a, err := netip.ParseAddr(host); err == nil {
			out = append(out, netip.AddrPortFrom(a, uint16(port)).String())
			continue
		}
		addrs, err := r.LookupNetIP(ctx, "ip", host)
		if err != nil {
			errs = append(errs, fmt.Errorf("local.peers %q: %w", p, err))
			continue
		}
		for _, a := range addrs {
			out = append(out, netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		}
	}
	return out, errs
}
