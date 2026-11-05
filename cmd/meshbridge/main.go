// Command meshbridge connects a MeshCore companion node to the SIERRA Mesh
// Relay. See docs/mesh-bridge-handoff.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/dpup/sierra-meshbridge/internal/bridge"
	"github.com/dpup/sierra-meshbridge/internal/companion"
	"github.com/dpup/sierra-meshbridge/internal/config"
	"github.com/dpup/sierra-meshbridge/internal/connect"
	"github.com/dpup/sierra-meshbridge/internal/nodelink"
	"github.com/dpup/sierra-meshbridge/internal/protocol"
	"github.com/dpup/sierra-meshbridge/internal/relayclient"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `meshbridge connects a MeshCore companion node to the SIERRA Mesh Relay.

Usage:
  meshbridge run [-config path]   Run the bridge in the foreground
  meshbridge find-node -host addr Connect to a TCP node and print its identity
  meshbridge connect <gateway>    Use a remote gateway as a local TCP companion port
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
	case "connect":
		err = cmdConnect(os.Args[2:])
	case "find-node":
		err = cmdFindNode(os.Args[2:])
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

// cmdFindNode runs the identity check against a TCP node. Serial detection
// and LAN scanning come later.
func cmdFindNode(args []string) error {
	fs := flag.NewFlagSet("find-node", flag.ExitOnError)
	host := fs.String("host", "", "node address")
	port := fs.Int("port", 5000, "node TCP port")
	_ = fs.Parse(args)
	if *host == "" {
		return errors.New("-host is required for now")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	link, err := nodelink.TCP{Host: *host, Port: *port}.Open(ctx)
	if err != nil {
		return err
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
		return fmt.Errorf("identity check against %s: %w", link.Detail(), err)
	}
	fmt.Printf("Found: %s\n  link    %s %s\n  pubkey  %s\n  model   %s\n  fw      %s\n",
		id.Name, link.Kind(), link.Detail(), id.Pubkey, id.Model, id.FW)
	return nil
}

// clientTokenEnv supplies the client token without putting it in shell history.
const clientTokenEnv = "MESHBRIDGE_CLIENT_TOKEN"

func cmdConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	token := fs.String("token", "", "client token (smc_…); default $"+clientTokenEnv)
	listen := fs.String("listen", "127.0.0.1:5000", "local address for companion apps")
	relay := fs.String("relay", config.DefaultRelay, "relay base URL")
	preempt := fs.Bool("preempt", false, "take the gateway from its current user (admins only)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: meshbridge connect <gateway> [flags]\n\n")
		fs.PrintDefaults()
	}
	// Accept the gateway before or after the flags.
	var gateway string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		gateway, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if gateway == "" && fs.NArg() > 0 {
		gateway = fs.Arg(0)
	}
	if gateway == "" {
		fs.Usage()
		os.Exit(2)
	}
	if *token == "" {
		*token = os.Getenv(clientTokenEnv)
	}
	if *token == "" {
		return fmt.Errorf("a client token is required (-token or $%s)", clientTokenEnv)
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fmt.Fprintf(os.Stderr, "WARNING: listening on %s. Anyone who can reach this address can use %s with YOUR token: companion apps have no authentication.\n", *listen, gateway)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	say := func(format string, a ...any) {
		fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
	}
	say("Listening on %s for %s. Point your MeshCore app or meshcore-cli at it (e.g. meshcore-cli -t %s -p %s).",
		ln.Addr(), gateway, host, portOf(ln.Addr()))
	shim := connect.New(connect.Options{
		Gateway: gateway,
		Say:     say,
		Dial: func(ctx context.Context) (*relayclient.Conn, error) {
			return relayclient.Dial(ctx, *relay, gateway, *token, *preempt)
		},
	})
	return shim.Serve(ctx, ln)
}

func portOf(a net.Addr) string {
	_, p, _ := net.SplitHostPort(a.String())
	return p
}
