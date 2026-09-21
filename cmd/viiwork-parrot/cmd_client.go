package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/janit/viiwork-parrot/internal/api"
	"github.com/janit/viiwork-parrot/internal/node"
	"github.com/janit/viiwork-parrot/internal/units"
)

func init() {
	register(command{"status", "show model states on the local node", runStatus})
	register(command{"limit", "show or temporarily override limits", runLimit})
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("api", "127.0.0.1:7950", "daemon API address")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	st, err := api.NewClient(*addr).Status(context.Background())
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(st)
	}
	return writeStatus(os.Stdout, st)
}

// writeStatus renders model statuses as a table. A model with Duplicates
// (further verified copies of its files found on this host — never deleted,
// since viiwork-parrot keeps a single seeded copy per host) gets an extra
// indented "duplicates:" line right after its row, so the paths are visible
// without needing --json.
func writeStatus(w io.Writer, st []node.ModelStatus) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tSTATE\tDONE\tDOWN\tUP\tPEERS\tPATH / ERROR")
	for _, m := range st {
		detail := m.Path
		if m.Error != "" {
			detail = m.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f%%\t%s\t%s\t%d\t%s\n", m.ID, m.State, m.Percent,
			units.FormatRate(m.DownRate), units.FormatRate(m.UpRate), m.Peers, detail)
		if len(m.Duplicates) > 0 {
			fmt.Fprintf(tw, "  duplicates: %s\n", strings.Join(m.Duplicates, ", "))
		}
	}
	return tw.Flush()
}

func runLimit(args []string) error {
	fs := flag.NewFlagSet("limit", flag.ExitOnError)
	addr := fs.String("api", "127.0.0.1:7950", "daemon API address")
	up := fs.String("up", "", "upload rate until the next schedule boundary (e.g. 20Mbit, 0 = unlimited)")
	down := fs.String("down", "", "download rate until the next schedule boundary")
	conns := fs.Int("conns", -1, "max established connections until the next schedule boundary")
	clear := fs.Bool("clear", false, "drop the temporary override")
	fs.Parse(args)
	c := api.NewClient(*addr)
	ctx := context.Background()
	switch {
	case *clear:
		if err := c.ClearOverride(ctx); err != nil {
			return err
		}
	case *up != "" || *down != "" || *conns >= 0:
		var o api.OverrideRequest
		if *up != "" {
			o.Upload = up
		}
		if *down != "" {
			o.Download = down
		}
		if *conns >= 0 {
			o.MaxConns = conns
		}
		if err := c.SetOverride(ctx, o); err != nil {
			return err
		}
	}
	ls, err := c.Limits(ctx)
	if err != nil {
		return err
	}
	rule := "base"
	if ls.Rule >= 0 {
		rule = fmt.Sprintf("schedule[%d]", ls.Rule)
	}
	e := ls.Effective
	fmt.Printf("upload %s  download %s  max_conns %d  per_torrent %d  active_uploads %d  (%s", units.FormatRate(e.Upload), units.FormatRate(e.Download), e.MaxConns, e.MaxConnsPerTorrent, e.MaxActiveUploads, rule)
	if ls.Override != nil {
		fmt.Print(" + override until next boundary")
	}
	fmt.Println(")")
	return nil
}
