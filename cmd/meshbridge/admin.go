package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dpup/sierra-meshrelay/internal/admin"
	"github.com/dpup/sierra-meshrelay/internal/config"
)

const adminUsage = `meshbridge admin: manage SIERRA pairing (needs an admin token).

Usage:
  meshbridge admin pairings                      Pending pairing codes
  meshbridge admin approve <code> [gateway]      Approve a code (default name: the node's)
        -repair                                  re-pair an existing gateway (rotates its token)
  meshbridge admin deny <code>                   Refuse a code
  meshbridge admin invite [gateway]              Create a passphrase invite and print a message to send
        -uses N -prefix NAME -days N -node PUBKEY -note TEXT
  meshbridge admin invites                       List invites
  meshbridge admin revoke <invite-id>            Revoke an invite
  meshbridge admin gateways                      Gateways and their status

The admin token comes from $MESHBRIDGE_ADMIN_TOKEN, or from
` + "~/.config/meshbridge/admin.token" + ` (chmod 600). Every command takes -relay.
`

func cmdAdmin(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(adminUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("admin "+sub, flag.ExitOnError)
	relayURL := fs.String("relay", config.DefaultRelay, "relay URL")
	tokenFile := fs.String("token-file", admin.DefaultTokenFile(), "admin token file (if $"+admin.TokenEnv+" is unset)")
	repair := fs.Bool("repair", false, "approve: re-pair an existing gateway (rotates its token)")
	uses := fs.Int("uses", 1, "invite: how many bridges it can set up")
	prefix := fs.String("prefix", "", "invite: name gateways <prefix>-1, -2… (multi-use)")
	days := fs.Float64("days", 7, "invite: days until it expires (max 30)")
	node := fs.String("node", "", "invite: only redeemable by the node with this pubkey")
	note := fs.String("note", "", "invite: a note for admins")
	pos := parseInterleaved(fs, args)

	tok, err := admin.LoadToken(*tokenFile)
	if err != nil {
		return err
	}
	c := &admin.Client{APIBase: config.HTTPBase(*relayURL), Token: tok}
	ctx := context.Background()
	arg := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}

	switch sub {
	case "pairings":
		ps, err := c.Pairings(ctx)
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			fmt.Println("No pairing codes waiting.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "CODE\tCOMPUTER\tNODE\tFROM\tAGE\tWOULD BE NAMED")
		for _, p := range ps {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.UserCode, dash(p.HostLabel), nodeLabel(p.Node.Name, p.Node.Model),
				p.IP, time.Since(p.CreatedAt).Round(time.Second), p.SuggestedGateway)
		}
		w.Flush()
		fmt.Println("\nApprove with: meshbridge admin approve <code> [gateway]")
		return nil

	case "approve":
		if arg(0) == "" {
			return fmt.Errorf("usage: meshbridge admin approve <code> [gateway]")
		}
		g, err := c.Approve(ctx, arg(0), arg(1), *repair)
		if err != nil {
			return err
		}
		verb := "Approved"
		if *repair {
			verb = "Re-paired"
		}
		fmt.Printf("%s %s as gateway %q. The bridge picks up its token within a few seconds.\n",
			verb, strings.ToUpper(arg(0)), g.Name)
		return nil

	case "deny":
		if arg(0) == "" {
			return fmt.Errorf("usage: meshbridge admin deny <code>")
		}
		if err := c.Deny(ctx, arg(0)); err != nil {
			return err
		}
		fmt.Printf("Denied %s.\n", strings.ToUpper(arg(0)))
		return nil

	case "invite":
		req := admin.InviteRequest{Gateway: arg(0), NamePrefix: *prefix, MaxUses: *uses,
			ExpiresInHours: *days * 24, ExpectedPubkey: *node, Note: *note}
		inv, err := c.CreateInvite(ctx, req)
		if err != nil {
			return err
		}
		name := inv.Invite.Gateway
		switch {
		case name == "" && inv.Invite.NamePrefix != "":
			name = inv.Invite.NamePrefix + "-1, -2…"
		case name == "":
			name = "named after the node"
		}
		fmt.Printf("Invite %s · gateway %s · %d use(s) · expires %s\n", inv.Invite.ID, name, inv.Invite.MaxUses,
			inv.Invite.ExpiresAt.Local().Format("Mon 2 Jan 15:04"))
		fmt.Printf("Passphrase: %s   (shown once)\n", strings.ReplaceAll(inv.Passphrase, "-", " "))
		fmt.Printf("Link:       %s\n\n", inv.Link)
		fmt.Println("Message to send:")
		fmt.Println("────────────────────────────────────────")
		fmt.Print(admin.InviteMessage(inv))
		fmt.Println("────────────────────────────────────────")
		return nil

	case "invites":
		invs, err := c.Invites(ctx)
		if err != nil {
			return err
		}
		if len(invs) == 0 {
			fmt.Println("No invites.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tGATEWAY\tUSES\tEXPIRES\tSTATE\tNOTE")
		for _, i := range invs {
			g := i.Gateway
			switch {
			case g == "" && i.NamePrefix != "":
				g = i.NamePrefix + "-N"
			case g == "":
				g = "(from node)"
			}
			state := "active"
			switch {
			case i.Revoked:
				state = "revoked"
			case !i.Active && i.Uses >= i.MaxUses:
				state = "used"
			case !i.Active:
				state = "expired"
			}
			fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\t%s\n", i.ID, dash(g), i.Uses, i.MaxUses,
				i.ExpiresAt.Local().Format("Mon 2 Jan"), state, i.Note)
		}
		w.Flush()
		return nil

	case "revoke":
		if arg(0) == "" {
			return fmt.Errorf("usage: meshbridge admin revoke <invite-id>")
		}
		if err := c.RevokeInvite(ctx, arg(0)); err != nil {
			return err
		}
		fmt.Printf("Revoked %s.\n", arg(0))
		return nil

	case "gateways":
		gs, err := c.Gateways(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "GATEWAY\tBRIDGE\tNODE\tVERSION\tLAST SEEN\tIN USE")
		for _, g := range gs {
			bridge := "offline"
			if g.Online {
				bridge = "online"
			}
			node := "-"
			if g.Node != nil {
				node = g.Node.State
				if g.Node.Name != "" {
					node += " (" + g.Node.Name + ")"
				}
			}
			if g.PubkeyStatus == "mismatch" {
				node += " ⚠ pubkey mismatch"
			}
			version := "-"
			if g.Agent != nil && g.Agent.Version != "" {
				version = g.Agent.Version + " " + g.Agent.OS
			}
			seen := "-"
			if g.LastSeen != nil {
				seen = ago(*g.LastSeen)
			}
			inUse := ""
			if g.Lease != nil {
				inUse = g.Lease.Client
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", g.Name, bridge, node, version, seen, inUse)
		}
		w.Flush()
		return nil
	}
	return fmt.Errorf("unknown admin command %q\n\n%s", sub, adminUsage)
}

// parseInterleaved parses flags that may come before or after positional
// arguments (`approve K7M-4QX cabin -repair`), returning the positionals.
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func nodeLabel(name, model string) string {
	switch {
	case name == "":
		return "(not found yet)"
	case model != "":
		return name + " · " + model
	}
	return name
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
