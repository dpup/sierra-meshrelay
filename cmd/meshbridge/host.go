package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/dpup/sierra-meshrelay/internal/companion"
	"github.com/dpup/sierra-meshrelay/internal/config"
	"github.com/dpup/sierra-meshrelay/internal/control"
	"github.com/dpup/sierra-meshrelay/internal/nodelink"
	"github.com/dpup/sierra-meshrelay/internal/pairing"
	"github.com/dpup/sierra-meshrelay/meshproto"
)

// identifyTCP runs the identity check against a TCP node.
func identifyTCP(host string, port int) (companion.Identity, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	link, err := nodelink.TCP{Host: host, Port: port}.Open(ctx)
	if err != nil {
		return companion.Identity{}, "", err
	}
	defer link.Close()
	frames := make(chan []byte, 64)
	go func() {
		defer close(frames)
		for {
			f, err := link.ReadFrame()
			if err != nil {
				return
			}
			frames <- f
		}
	}()
	id, err := companion.Identify(ctx, frames, link.WriteFrame, 5*time.Second)
	if err != nil {
		return id, link.Detail(), fmt.Errorf("identity check against %s: %w", link.Detail(), err)
	}
	return id, link.Detail(), nil
}

// --- setup -------------------------------------------------------------------

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "config file to write")
	passphrase := fs.String("passphrase", "", "the four words from your SIERRA contact (omit to be asked, or to get a pairing code)")
	relay := fs.String("relay", "", "relay URL (default: the config's, or SIERRA's)")
	host := fs.String("host", "", "your node's address on this network (WiFi/Ethernet nodes)")
	port := fs.Int("port", 5000, "your node's TCP port")
	repair := fs.Bool("repair", false, "pair again even though this bridge is already paired")
	_ = fs.Parse(args)

	cfg, exists, err := config.LoadRaw(*path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *path, err)
	}
	if exists && cfg.Token != "" && !*repair {
		return fmt.Errorf("this bridge is already paired as gateway %q (%s). Use -repair to pair it again", cfg.Gateway, *path)
	}
	if *relay != "" {
		cfg.Relay = *relay
	}
	if *host != "" {
		cfg.Node.TCPHost, cfg.Node.TCPPort, cfg.Node.Link = *host, *port, config.LinkTCP
	}

	req := meshproto.PairStartRequest{
		Agent: meshproto.Agent{Name: "meshbridge", Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH},
		Node:  meshproto.PairNode{Node: meshproto.Node{State: meshproto.NodeDisconnected}},
	}
	if h, err := os.Hostname(); err == nil {
		req.HostLabel = h
	}
	if cfg.Node.TCPHost != "" {
		fmt.Printf("Looking for your node at %s:%d…\n", cfg.Node.TCPHost, cfg.Node.TCPPort)
		id, detail, err := identifyTCP(cfg.Node.TCPHost, cfg.Node.TCPPort)
		if err != nil {
			fmt.Printf("Couldn't reach it (%v). Pairing anyway; the bridge will keep looking.\n\n", err)
		} else {
			fmt.Printf("Found: %s · %s (%s)\n\n", id.Name, id.Model, detail)
			req.Node = meshproto.PairNode{Node: meshproto.Node{State: meshproto.NodeConnected,
				Pubkey: id.Pubkey, Name: id.Name, FW: id.FW, Model: id.Model}, Link: meshproto.LinkTCP}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := &pairing.Client{APIBase: config.HTTPBase(cfg.Relay)}
	interactive := isTerminal(os.Stdin)
	in := bufio.NewReader(os.Stdin)

	phrase := *passphrase
	if phrase == "" && interactive {
		fmt.Print("Got a passphrase from SIERRA? Type it here, or press Enter for a pairing code: ")
		line, _ := in.ReadString('\n')
		phrase = strings.TrimSpace(line)
	}
	var res meshproto.PairResult
	if phrase != "" {
		req.Passphrase = phrase
		res, err = client.Redeem(ctx, req)
		switch {
		case errors.Is(err, pairing.ErrInvalidPassphrase) && interactive:
			fmt.Println("That passphrase didn't work. Check the words, or ask your SIERRA contact for a new one.")
			fmt.Println("Getting a pairing code instead…")
		case err != nil:
			return err
		}
	}
	for res.Token == "" {
		start, err := client.Start(ctx, req)
		if err != nil {
			return err
		}
		fmt.Printf("\n    Your pairing code:   %s\n\n", start.UserCode)
		fmt.Printf("Text this code to your SIERRA contact. Waiting for them to approve it (the code lasts %d minutes)…\n",
			start.ExpiresIn/60)
		res, err = client.Wait(ctx, start)
		if errors.Is(err, pairing.ErrExpired) && interactive {
			fmt.Print("The code expired. Press Enter for a new one, or Ctrl-C to stop: ")
			_, _ = in.ReadString('\n')
			continue
		}
		if err != nil {
			return err
		}
	}

	cfg.Gateway, cfg.Token = res.Gateway, res.Token
	if res.Relay != "" {
		cfg.Relay = res.Relay
	}
	if res.ExpectedPubkey != "" {
		cfg.Node.ExpectedPubkey = res.ExpectedPubkey
	}
	if err := config.Save(*path, cfg); err != nil {
		return fmt.Errorf("paired as %q, but couldn't save the config to %s: %w", res.Gateway, *path, err)
	}
	fmt.Printf("\nConnected to SIERRA as gateway %q. Config saved to %s.\n", res.Gateway, *path)
	if cfg.Node.TCPHost == "" {
		fmt.Println("Next, set node.tcp_host in that file to your node's address (auto-detect is coming).")
	}
	fmt.Printf("Start the bridge with: meshbridge run -config %s\n", *path)
	return nil
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// --- status, pause, resume -----------------------------------------------------

// controlClient finds the running bridge's control API from its config path.
func controlClient(fs *flag.FlagSet, args []string) (*control.Client, *flag.FlagSet, error) {
	path := fs.String("config", config.DefaultPath(), "config file of the running bridge")
	addr := fs.String("control", control.DefaultAddr, "the bridge's control API address")
	_ = fs.Parse(args)
	tok, err := control.ReadToken(filepath.Join(filepath.Dir(*path), control.TokenFile))
	if err != nil {
		return nil, fs, fmt.Errorf("can't read the bridge's control secret (is it running, and can you read %s?): %w",
			filepath.Dir(*path), err)
	}
	return &control.Client{Addr: *addr, Token: tok}, fs, nil
}

func cmdStatus(args []string) error {
	c, _, err := controlClient(flag.NewFlagSet("status", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	d, err := c.Do(context.Background(), "GET", "/status", nil)
	if err != nil {
		return notRunning(err)
	}
	printStatus(d)
	return nil
}

func cmdPause(args []string) error {
	c, fs, err := controlClient(flag.NewFlagSet("pause", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	until := "1h"
	if fs.NArg() > 0 {
		until = fs.Arg(0)
	}
	d, err := c.Do(context.Background(), "POST", "/pause", control.PauseRequest{Until: until})
	if err != nil {
		return notRunning(err)
	}
	printStatus(d)
	return nil
}

func cmdResume(args []string) error {
	c, _, err := controlClient(flag.NewFlagSet("resume", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	d, err := c.Do(context.Background(), "POST", "/resume", nil)
	if err != nil {
		return notRunning(err)
	}
	printStatus(d)
	return nil
}

func notRunning(err error) error {
	if strings.Contains(err.Error(), "connection refused") {
		return errors.New("the bridge isn't running (start it with meshbridge run)")
	}
	return err
}

func printStatus(d control.StatusDoc) {
	dot := map[string]string{control.Green: "\033[32m●\033[0m", control.Amber: "\033[33m●\033[0m", control.Red: "\033[31m●\033[0m"}[d.Level]
	if !isTerminal(os.Stdout) {
		dot = "[" + d.Level + "]"
	}
	fmt.Printf("%s %s\n", dot, d.Reason)
	fmt.Printf("  Gateway:   %s\n", d.Gateway)
	node := d.Node.State
	if d.Node.Name != "" {
		node = d.Node.Name
		if d.Node.Model != "" {
			node += " (" + d.Node.Model + ")"
		}
		if d.Node.Link != "" {
			node += " · " + d.Node.Link
		}
	}
	fmt.Printf("  Node:      %s\n", node)
	if d.LastMeshTraffic != nil {
		fmt.Printf("  Last mesh traffic: %s ago\n", time.Since(*d.LastMeshTraffic).Round(time.Second))
	}
	switch {
	case d.Paused && d.PauseUntil == nil:
		fmt.Println("  Sharing:   paused until you resume")
	case d.Paused:
		fmt.Printf("  Sharing:   paused until %s\n", d.PauseUntil.Local().Format("Mon 15:04"))
	case d.ClientAttached:
		fmt.Println("  Sharing:   on (a SIERRA client is using the node now)")
	default:
		fmt.Println("  Sharing:   on")
	}
}
