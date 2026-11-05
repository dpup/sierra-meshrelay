// Package config loads the bridge's TOML config file.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultRelay is the production relay's base URL.
const DefaultRelay = "wss://data.sierragridteam.org/mesh/relay"

// Link modes.
const (
	LinkAuto   = "auto"
	LinkSerial = "serial"
	LinkTCP    = "tcp"
)

// Config is the bridge config. See docs/mesh-bridge-handoff.md, "Config".
type Config struct {
	Gateway string `toml:"gateway"`
	Relay   string `toml:"relay"`
	Token   string `toml:"token"`
	Node    Node   `toml:"node"`
	Host    Host   `toml:"host"`
	Log     Log    `toml:"log"`
}

// Node configures the node link.
type Node struct {
	Link           string `toml:"link"`
	ExpectedPubkey string `toml:"expected_pubkey"`
	SerialPort     string `toml:"serial_port"`
	Baud           int    `toml:"baud"`
	TCPHost        string `toml:"tcp_host"`
	TCPPort        int    `toml:"tcp_port"`
}

// Host configures host-machine behaviour.
type Host struct {
	KeepAwake bool `toml:"keep_awake"`
}

// Log configures logging.
type Log struct {
	Level string `toml:"level"`
}

// TokenEnv overrides the config file's token, so developers and containers
// can keep the secret out of the file.
const TokenEnv = "MESHBRIDGE_TOKEN"

// Default returns a config with every default filled in.
func Default() Config {
	return Config{
		Relay: DefaultRelay,
		Node:  Node{Link: LinkAuto, Baud: 115200, TCPPort: 5000},
		Host:  Host{KeepAwake: true},
		Log:   Log{Level: "info"},
	}
}

// Load reads path over the defaults, applies env overrides and validates.
func Load(path string) (Config, error) {
	c := Default()
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return c, fmt.Errorf("reading config %s: %w", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return c, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if t := os.Getenv(TokenEnv); t != "" {
		c.Token = t
	}
	return c, c.Validate()
}

var gatewayRe = regexp.MustCompile(`^[a-z0-9-]{3,40}$`)
var pubkeyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the config is usable.
func (c *Config) Validate() error {
	var errs []error
	if !gatewayRe.MatchString(c.Gateway) {
		errs = append(errs, fmt.Errorf("gateway %q must match [a-z0-9-]{3,40}", c.Gateway))
	}
	if !strings.HasPrefix(c.Relay, "wss://") && !strings.HasPrefix(c.Relay, "ws://") {
		errs = append(errs, fmt.Errorf("relay %q must be a ws:// or wss:// URL", c.Relay))
	}
	c.Relay = strings.TrimRight(c.Relay, "/")
	if c.Token == "" {
		errs = append(errs, fmt.Errorf("token is required (or set %s)", TokenEnv))
	} else if !strings.HasPrefix(c.Token, "smb_") {
		errs = append(errs, errors.New("token must be a bridge token (smb_…)"))
	}
	c.Node.ExpectedPubkey = strings.ToLower(strings.TrimSpace(c.Node.ExpectedPubkey))
	if c.Node.ExpectedPubkey != "" && !pubkeyRe.MatchString(c.Node.ExpectedPubkey) {
		errs = append(errs, errors.New("node.expected_pubkey must be 64 hex characters"))
	}
	switch c.Node.Link {
	case LinkAuto, LinkTCP:
	case LinkSerial:
		errs = append(errs, errors.New("node.link = \"serial\" isn't supported yet (M2)"))
	default:
		errs = append(errs, fmt.Errorf("node.link %q must be auto, serial or tcp", c.Node.Link))
	}
	if c.Node.TCPPort <= 0 || c.Node.TCPPort > 65535 {
		errs = append(errs, fmt.Errorf("node.tcp_port %d is out of range", c.Node.TCPPort))
	}
	return errors.Join(errs...)
}

// DefaultPath is the config file location for this OS.
func DefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return pd + `\SIERRA\meshbridge\meshbridge.toml`
	case "darwin":
		return "/Library/Application Support/SIERRA/meshbridge/meshbridge.toml"
	default:
		return "/etc/meshbridge/meshbridge.toml"
	}
}
