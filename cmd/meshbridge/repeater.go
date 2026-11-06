package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/dpup/sierra-meshrelay/internal/admin"
	"github.com/dpup/sierra-meshrelay/internal/config"
)

const repeaterUsage = `meshbridge repeater: share your MeshCore repeaters with SIERRA.

SIERRA logs in to a shared repeater over the mesh to read its status
(battery, airtime, packet counts) and report it on the Grid.

Usage:
  meshbridge repeater add [name or pubkey]   Pick a repeater your node knows and share it
  meshbridge repeater list                   Repeaters this bridge has shared
  meshbridge repeater remove <name or pubkey>
                                             Stop sharing one (deletes its password from SIERRA)

Run them as root (sudo) or an administrator: they use this bridge's
credentials from its config. -config picks another config file.
`

const repeaterWarning = `Before you share it:

  • SIERRA stores the password (encrypted) and uses it to log in and read
    the repeater's status.
  • Any SIERRA gateway in range of the repeater may be used to log in to
    it, not just yours.
  • Use the repeater's guest password if it has one: a guest can only read
    status. An admin password works too, but gives full control.
  • You can stop sharing it at any time with: meshbridge repeater remove
`

func cmdRepeater(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(repeaterUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("repeater "+sub, flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "bridge config file")
	yes := fs.Bool("yes", false, "add: don't ask for confirmation")
	pos := parseInterleaved(fs, args)
	cfg, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("%w\nRun this as root (sudo) or an administrator on the bridge's computer", err)
	}
	c := &admin.Client{APIBase: config.HTTPBase(cfg.Relay), Token: cfg.Token}
	ctx := context.Background()
	in := bufio.NewReader(os.Stdin)

	switch sub {
	case "list", "ls":
		rs, err := c.MyRepeaters(ctx)
		if err != nil {
			return err
		}
		if len(rs) == 0 {
			fmt.Println("This bridge hasn't shared any repeaters. Share one with: meshbridge repeater add")
			return nil
		}
		printRepeaters(rs, false)
		return nil

	case "remove", "rm":
		if len(pos) == 0 {
			return fmt.Errorf("usage: meshbridge repeater remove <name or pubkey>")
		}
		rs, err := c.MyRepeaters(ctx)
		if err != nil {
			return err
		}
		r, err := pickShared(rs, pos[0])
		if err != nil {
			return err
		}
		if err := c.UnshareRepeater(ctx, r.Pubkey); err != nil {
			return err
		}
		fmt.Printf("Stopped sharing %s. SIERRA no longer has its password.\n", r.Name)
		return nil

	case "add":
		fmt.Println("Asking your node which repeaters it has heard…")
		near, err := c.NearbyRepeaters(ctx)
		if err != nil {
			return err
		}
		pick, err := pickNearby(near, strings.Join(pos, " "), in)
		if err != nil {
			return err
		}
		fmt.Printf("\nSharing %s (%s…)\n\n", pick.Name, pick.Pubkey[:12])
		fmt.Print(repeaterWarning)
		if !*yes {
			fmt.Print("\nShare it? [y/N]: ")
			line, _ := in.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				fmt.Println("Not shared.")
				return nil
			}
		}
		pw, err := readPassword(in, "Repeater password (guest if it has one; Enter for none): ")
		if err != nil {
			return err
		}
		fmt.Println("Logging in to the repeater over the mesh (this can take up to a minute)…")
		res, err := c.ShareRepeater(ctx, pick.Pubkey, pick.Name, pw)
		if err != nil {
			return err
		}
		fmt.Printf("\nShared %s with SIERRA. Logged in as %s", res.Repeater.Name, res.Check.Access)
		if st := res.Check.Status; st != nil && st.BatteryMilliVolts != nil {
			fmt.Printf("; battery %.2f V", float64(*st.BatteryMilliVolts)/1000)
		}
		fmt.Println(".")
		if res.Check.Access == "admin" {
			fmt.Println("\nThat was the admin password. SIERRA only reads status, so a guest password is")
			fmt.Println("enough: set one on the repeater (set guest.password …), then run this again with it.")
		}
		return nil
	}
	return fmt.Errorf("unknown repeater command %q\n\n%s", sub, repeaterUsage)
}

// pickNearby chooses a repeater: by name or pubkey prefix if given, else by
// asking.
func pickNearby(near []admin.NearbyRepeater, want string, in *bufio.Reader) (admin.NearbyRepeater, error) {
	if len(near) == 0 {
		return admin.NearbyRepeater{}, fmt.Errorf("your node hasn't heard any repeaters yet. A repeater shows up once your node hears its advert; " +
			"send a flood advert from the repeater (or wait for its next one) and try again")
	}
	if want != "" {
		var hits []admin.NearbyRepeater
		for _, r := range near {
			if strings.EqualFold(r.Name, want) || strings.HasPrefix(r.Pubkey, strings.ToLower(want)) {
				hits = append(hits, r)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0], nil
		case 0:
			return admin.NearbyRepeater{}, fmt.Errorf("your node doesn't know a repeater called %q", want)
		}
		near = hits
	}
	fmt.Println("\nRepeaters your node has heard:")
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for i, r := range near {
		heard, shared := "-", ""
		if r.LastAdvert != nil {
			heard = ago(*r.LastAdvert)
		}
		if r.Shared {
			shared = "(already shared)"
		}
		fmt.Fprintf(w, "  %d.\t%s\t%s…\tadvert %s\t%s\n", i+1, r.Name, r.Pubkey[:12], heard, shared)
	}
	w.Flush()
	if !isTerminal(os.Stdin) {
		return admin.NearbyRepeater{}, fmt.Errorf("name the repeater: meshbridge repeater add <name or pubkey>")
	}
	for {
		fmt.Printf("Which one? [1-%d]: ", len(near))
		line, err := in.ReadString('\n')
		if n, perr := strconv.Atoi(strings.TrimSpace(line)); perr == nil && n >= 1 && n <= len(near) {
			return near[n-1], nil
		}
		if err != nil {
			return admin.NearbyRepeater{}, fmt.Errorf("no repeater chosen")
		}
	}
}

func pickShared(rs []admin.Repeater, want string) (admin.Repeater, error) {
	var hits []admin.Repeater
	for _, r := range rs {
		if strings.EqualFold(r.Name, want) || strings.HasPrefix(r.Pubkey, strings.ToLower(want)) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return admin.Repeater{}, fmt.Errorf("no shared repeater matches %q", want)
	}
	return admin.Repeater{}, fmt.Errorf("%q matches %d repeaters; use more of the pubkey", want, len(hits))
}

// readPassword reads a password without echoing it when stdin is a terminal,
// or from $MESHBRIDGE_REPEATER_PASSWORD (for scripts: never a flag, which
// other users could see in the process list).
func readPassword(in *bufio.Reader, prompt string) (string, error) {
	if pw, ok := os.LookupEnv("MESHBRIDGE_REPEATER_PASSWORD"); ok {
		return pw, nil
	}
	fmt.Print(prompt)
	if isTerminal(os.Stdin) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return strings.TrimRight(string(b), "\r\n"), err
	}
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no password given")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func printRepeaters(rs []admin.Repeater, withOwner bool) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	hdr := "REPEATER\tPUBKEY\tACCESS\tLAST READ\tVIA\tPROBLEM"
	if withOwner {
		hdr += "\tSHARED BY"
	}
	fmt.Fprintln(w, hdr)
	for _, r := range rs {
		read := "never"
		if r.LastOKAt != nil {
			read = ago(*r.LastOKAt)
		}
		problem := ""
		if r.LastError != "" && (r.LastOKAt == nil || (r.LastAttemptAt != nil && r.LastAttemptAt.After(*r.LastOKAt))) {
			problem = r.LastError
		}
		line := fmt.Sprintf("%s\t%s…\t%s\t%s\t%s\t%s", r.Name, r.Pubkey[:12], r.Access, read, dash(r.LastGateway), problem)
		if withOwner {
			by := r.AddedByGateway
			if by == "" {
				by = r.AddedByClient + " (admin)"
			}
			line += "\t" + by
		}
		fmt.Fprintln(w, line)
	}
	w.Flush()
}
