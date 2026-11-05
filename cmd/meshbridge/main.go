// Command meshbridge connects a MeshCore companion node to the SIERRA Mesh
// Relay. See docs/mesh-bridge-handoff.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	"github.com/dpup/sierra-meshbridge/internal/bridge"
	"github.com/dpup/sierra-meshbridge/internal/config"
	"github.com/dpup/sierra-meshbridge/internal/nodelink"
	"github.com/dpup/sierra-meshbridge/internal/protocol"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `meshbridge connects a MeshCore companion node to the SIERRA Mesh Relay.

Usage:
  meshbridge run [-config path]   Run the bridge in the foreground
  meshbridge version              Print version and build info
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "version", "-version", "--version":
		cmdVersion()
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "meshbridge: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshbridge:", err)
		os.Exit(1)
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "config file")
	logJSON := fs.Bool("log-json", false, "log as JSON")
	_ = fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log.Level, *logJSON)

	if cfg.Node.TCPHost == "" {
		// LAN scanning and serial auto-detect arrive in later milestones.
		return errors.New("node.tcp_host is required for now (auto-detect isn't built yet)")
	}
	opener := nodelink.TCP{Host: cfg.Node.TCPHost, Port: cfg.Node.TCPPort}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("meshbridge starting", "version", version, "gateway", cfg.Gateway, "relay", cfg.Relay,
		"node", fmt.Sprintf("%s:%d", cfg.Node.TCPHost, cfg.Node.TCPPort))
	b := bridge.New(bridge.Options{
		Config: cfg, Opener: opener, Log: log, Version: version,
		LinkKind: protocol.LinkTCP, LinkDetail: fmt.Sprintf("%s:%d", cfg.Node.TCPHost, cfg.Node.TCPPort),
	})
	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("meshbridge stopped")
	return nil
}

func newLogger(level string, asJSON bool) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if asJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func cmdVersion() {
	fmt.Printf("meshbridge %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" || s.Key == "vcs.modified" {
				fmt.Printf("  %s=%s\n", s.Key, s.Value)
			}
		}
	}
}
