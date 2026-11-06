// Package admin is a small client for the relay's admin API (pairings,
// invites, gateways), behind `meshbridge admin`. It defines its own response
// types instead of importing the relay package, so the bridge binary stays
// free of the relay's dependencies.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dpup/sierra-meshrelay/meshproto"
)

// TokenEnv supplies the admin token.
const TokenEnv = "MESHBRIDGE_ADMIN_TOKEN"

// DefaultTokenFile is where the admin token is read from when TokenEnv is
// unset: ~/.config/meshbridge/admin.token.
func DefaultTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "meshbridge", "admin.token")
}

// LoadToken returns the admin token from TokenEnv, else from file.
func LoadToken(file string) (string, error) {
	if t := strings.TrimSpace(os.Getenv(TokenEnv)); t != "" {
		return t, nil
	}
	if file == "" {
		return "", fmt.Errorf("no admin token: set %s", TokenEnv)
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no admin token: set %s, or save it to %s (chmod 600)", TokenEnv, file)
	}
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(file); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by others; chmod 600 it\n", file)
	}
	return strings.TrimSpace(string(b)), nil
}

// Client calls the admin API at APIBase (https://…/mesh/relay/api).
type Client struct {
	APIBase string
	Token   string
	HTTP    *http.Client
}

// APIError is an error answer from the admin API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("relay answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if resp.StatusCode == http.StatusUnauthorized && e.Message == "" {
			e.Message = "the relay didn't accept the admin token"
		}
		return &APIError{Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Pairing is a device code waiting for approval.
type Pairing struct {
	UserCode         string             `json:"user_code"`
	HostLabel        string             `json:"host_label"`
	IP               string             `json:"ip"`
	Agent            meshproto.Agent    `json:"agent"`
	Node             meshproto.PairNode `json:"node"`
	SuggestedGateway string             `json:"suggested_gateway"`
	CreatedAt        time.Time          `json:"created_at"`
	ExpiresAt        time.Time          `json:"expires_at"`
}

// Pairings lists pending device codes.
func (c *Client) Pairings(ctx context.Context) ([]Pairing, error) {
	var out struct {
		Pairings []Pairing `json:"pairings"`
	}
	return out.Pairings, c.do(ctx, http.MethodGet, "/pairings", nil, &out)
}

// Gateway is one gateway's status.
type Gateway struct {
	Name         string                   `json:"name"`
	Notes        string                   `json:"notes"`
	Online       bool                     `json:"online"`
	LastSeen     *time.Time               `json:"last_seen"`
	Agent        *meshproto.Agent         `json:"agent"`
	Link         *meshproto.Link          `json:"link"`
	Node         *meshproto.Node          `json:"node"`
	PubkeyStatus string                   `json:"pubkey_status"`
	PinnedPubkey string                   `json:"pinned_pubkey"`
	Lease        *struct{ Client string } `json:"lease"`
}

// Approve approves a code onto gateway ("" lets the relay name it after the
// node). repair re-pairs an existing gateway, rotating its token.
func (c *Client) Approve(ctx context.Context, code, gateway string, repair bool) (Gateway, error) {
	var out struct {
		Gateway Gateway `json:"gateway"`
	}
	body := map[string]any{"repair": repair}
	if gateway != "" {
		body["gateway"] = gateway
	}
	return out.Gateway, c.do(ctx, http.MethodPost, "/pairings/"+url.PathEscape(code)+"/approve", body, &out)
}

// Deny refuses a code.
func (c *Client) Deny(ctx context.Context, code string) error {
	return c.do(ctx, http.MethodPost, "/pairings/"+url.PathEscape(code)+"/deny", nil, nil)
}

// InviteRequest creates a passphrase invite. Leave Gateway and NamePrefix
// empty to name the gateway after the node when it's redeemed.
type InviteRequest struct {
	Gateway        string  `json:"gateway,omitempty"`
	NamePrefix     string  `json:"name_prefix,omitempty"`
	MaxUses        int     `json:"max_uses,omitempty"`
	ExpiresInHours float64 `json:"expires_in_hours,omitempty"`
	ExpectedPubkey string  `json:"expected_pubkey,omitempty"`
	Note           string  `json:"note,omitempty"`
}

// Invite is a passphrase invite (never the passphrase itself).
type Invite struct {
	ID             string    `json:"id"`
	Gateway        string    `json:"gateway"`
	NamePrefix     string    `json:"name_prefix"`
	MaxUses        int       `json:"max_uses"`
	Uses           int       `json:"uses"`
	ExpiresAt      time.Time `json:"expires_at"`
	ExpectedPubkey string    `json:"expected_pubkey"`
	Note           string    `json:"note"`
	Revoked        bool      `json:"revoked"`
	Active         bool      `json:"active"`
	CreatedBy      string    `json:"created_by"`
}

// NewInvite is a created invite with its passphrase, shown once.
type NewInvite struct {
	Invite     Invite `json:"invite"`
	Passphrase string `json:"passphrase"`
	Link       string `json:"link"`
}

// CreateInvite creates an invite.
func (c *Client) CreateInvite(ctx context.Context, req InviteRequest) (NewInvite, error) {
	var out NewInvite
	return out, c.do(ctx, http.MethodPost, "/invites", req, &out)
}

// Invites lists invites.
func (c *Client) Invites(ctx context.Context) ([]Invite, error) {
	var out struct {
		Invites []Invite `json:"invites"`
	}
	return out.Invites, c.do(ctx, http.MethodGet, "/invites", nil, &out)
}

// RevokeInvite revokes an invite.
func (c *Client) RevokeInvite(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/invites/"+url.PathEscape(id), nil, nil)
}

// Gateways lists gateways with their status.
func (c *Client) Gateways(ctx context.Context) ([]Gateway, error) {
	var out struct {
		Gateways []Gateway `json:"gateways"`
	}
	return out.Gateways, c.do(ctx, http.MethodGet, "/gateways", nil, &out)
}

// InstallOneLiner is the Linux install command for a passphrase.
func InstallOneLiner(passphrase string) string {
	return `curl -fsSL https://raw.githubusercontent.com/dpup/sierra-meshrelay/main/scripts/install.sh | sh -s -- --passphrase "` +
		strings.ReplaceAll(passphrase, "-", " ") + `"`
}

// InviteMessage is a message to send the host with everything they need.
func InviteMessage(inv NewInvite) string {
	words := strings.ReplaceAll(inv.Passphrase, "-", " ")
	var b strings.Builder
	b.WriteString("Here's how to connect your MeshCore node to SIERRA.\n\n")
	b.WriteString("Your SIERRA passphrase is:  " + words + "\n\n")
	b.WriteString("On a Linux computer or Raspberry Pi on the same network as your node, run:\n\n")
	b.WriteString("  " + InstallOneLiner(inv.Passphrase) + "\n\n")
	fmt.Fprintf(&b, "The passphrase works until %s.", inv.Invite.ExpiresAt.Local().Format("Mon 2 Jan"))
	if inv.Invite.MaxUses > 1 {
		fmt.Fprintf(&b, " It can set up %d bridges.", inv.Invite.MaxUses)
	}
	b.WriteString("\n")
	return b.String()
}
