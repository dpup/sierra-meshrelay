// Package meshproto holds the SIERRA Mesh Relay wire protocol v1 types and
// constants. PROTOCOL.md, in this repo, is the spec they implement.
//
// The relay (package relay) and the bridge (cmd/meshbridge) in this repo both
// import this package so their message shapes can't drift. It has no
// dependencies and does no I/O, so it also suits small or embedded bridges.
package meshproto

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Subprotocol names carry the protocol's major version.
const (
	BridgeSubprotocol = "sierra-mesh-bridge.v1"
	ClientSubprotocol = "sierra-mesh-client.v1"
	Version           = 1
)

// Size limits.
const (
	// MaxFrameBytes is the largest binary message: one companion frame payload.
	// Current MeshCore firmware frames are at most 176 bytes.
	MaxFrameBytes = 512
	// MaxControlBytes is the largest text (JSON control) message. A larger
	// one is closed with 1009.
	MaxControlBytes = 8 << 10
)

// WebSocket close codes.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001 // the relay is restarting
	CloseMessageTooBig   = 1009 // text message over MaxControlBytes
	CloseBadToken        = 4001 // bad or revoked token
	CloseGatewayMismatch = 4002 // gateway name doesn't match the token
	CloseProtocolError   = 4003 // protocol error or missing hello
	CloseUnsupported     = 4004 // unsupported protocol version
	CloseReplaced        = 4009 // a newer session for this gateway connected
	CloseBusy            = 4029 // (client) the lease is held by someone else
)

// Node states, in hello.node, node_status and gateway_status.
const (
	NodeConnected    = "connected"
	NodeDisconnected = "disconnected"
	NodeMismatch     = "mismatch" // pubkey doesn't match the bridge's expected_pubkey
	NodePaused       = "paused"   // the host paused sharing; see Node.Until
)

// Link kinds, in hello.link.
const (
	LinkSerial = "serial"
	LinkTCP    = "tcp"
)

// Bridge states, in gateway_status.
const (
	BridgeOnline  = "online"
	BridgeOffline = "offline"
)

// Error codes. Receivers act on the code only; Error.Message is free text.
const (
	ErrHelloRequired       = "hello_required"
	ErrUnsupportedProtocol = "unsupported_protocol"
	ErrFrameTooLarge       = "frame_too_large"
	ErrBusy                = "busy"
	ErrGatewayOffline      = "gateway_offline"
	ErrPubkeyMismatch      = "pubkey_mismatch"
	ErrPubkeyUnverified    = "pubkey_unverified"
	ErrRenewDenied         = "renew_denied"
	ErrUnknownLease        = "unknown_lease"
)

// NodeStateErr is the error code for a gateway whose node is in state
// (node_disconnected, node_paused, ...).
func NodeStateErr(state string) string { return "node_" + state }

// lease_ending reasons. Receivers must treat an unknown reason as "the lease
// is ending".
const (
	ReasonExpiry         = "expiry"
	ReasonPreempted      = "preempted"
	ReasonGatewayOffline = "gateway_offline"
	ReasonIdle           = "idle"
	ReasonRevoked        = "revoked"
	ReasonPubkeyMismatch = "pubkey_mismatch"
	ReasonRelayShutdown  = "relay_shutdown"
	ReasonHostPaused     = "host_paused"
)

// Control message types.
const (
	TypeHello           = "hello"
	TypeWelcome         = "welcome"
	TypeNodeStatus      = "node_status"
	TypeStats           = "stats"
	TypeAttach          = "attach"
	TypeDetach          = "detach"
	TypeRelink          = "relink"
	TypeUpdateAvailable = "update_available"
	TypeError           = "error"
	TypeLease           = "lease"
	TypeLeaseEnding     = "lease_ending"
	TypeRenew           = "renew"
	TypeGatewayStatus   = "gateway_status"
)

// Envelope is the part every control message shares. Decode into it to route
// a message by Type; unknown types must be ignored.
type Envelope struct {
	Type string `json:"type"`
}

// TypeOf returns a control message's type, or "" if data isn't a JSON object
// with a string type field.
func TypeOf(data []byte) string {
	var env Envelope
	if json.Unmarshal(data, &env) != nil {
		return ""
	}
	return env.Type
}

// Agent identifies the bridge software (hello.agent).
type Agent struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	OS      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
}

// Link describes how the bridge reaches its node (hello.link).
type Link struct {
	Kind   string `json:"kind,omitempty"`   // LinkSerial or LinkTCP
	Detail string `json:"detail,omitempty"` // informational: port name or host:port
}

// Node is hello.node, the body of node_status, and lease.node and
// gateway_status.node. Only State is required; the identity fields may be
// omitted unless the node is connected.
type Node struct {
	State  string `json:"state"`
	Pubkey string `json:"pubkey,omitempty"` // 64 lowercase hex chars
	Name   string `json:"name,omitempty"`
	FW     string `json:"fw,omitempty"`
	Model  string `json:"model,omitempty"`
	Until  string `json:"until,omitempty"` // RFC 3339; with NodePaused
	Error  string `json:"error,omitempty"` // short reason, e.g. "serial port closed"
}

// Hello is the bridge's first message.
type Hello struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol"`
	Gateway  string `json:"gateway"`
	Agent    Agent  `json:"agent"`
	Link     Link   `json:"link"`
	Node     Node   `json:"node"`
}

// Welcome is the relay's reply to hello. The bridge sends no binary messages
// before it.
type Welcome struct {
	Type          string `json:"type"`
	Session       string `json:"session"`
	PingIntervalS int    `json:"ping_interval_s"`
	ServerTime    string `json:"server_time"`
}

// NodeStatus reports a node change (bridge → relay). Node's fields are
// flattened into the message.
type NodeStatus struct {
	Type string `json:"type"`
	Node
}

// Stats is the bridge's counters.
type Stats struct {
	UptimeS            int64 `json:"uptime_s"`
	NodeLinkReconnects int64 `json:"node_link_reconnects"`
	FramesToNode       int64 `json:"frames_to_node"`
	FramesFromNode     int64 `json:"frames_from_node"`
	RelayReconnects    int64 `json:"relay_reconnects"`
}

// StatsMsg is the periodic stats message (bridge → relay, every 60 s).
type StatsMsg struct {
	Type string `json:"type"`
	Stats
}

// Attach tells the bridge a client started using the node.
type Attach struct {
	Type   string `json:"type"`
	Lease  string `json:"lease"`
	Client string `json:"client"`
}

// Detach tells the bridge a client stopped using the node.
type Detach struct {
	Type  string `json:"type"`
	Lease string `json:"lease"`
}

// Relink asks the bridge to reopen its node link and re-run the identity check.
type Relink struct {
	Type string `json:"type"`
}

// UpdateAvailable advertises a bridge update. v1 bridges may only log it.
type UpdateAvailable struct {
	Type    string `json:"type"`
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Sig     string `json:"sig"` // base64 ed25519
}

// Error reports an error (either direction).
type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"` // free text: never display or depend on it
}

// Lease grants (or renews) a lease (relay → client).
type Lease struct {
	Type      string `json:"type"`
	Lease     string `json:"lease"`
	Gateway   string `json:"gateway"`
	ExpiresAt string `json:"expires_at"`
	Node      *Node  `json:"node,omitempty"`
}

// LeaseEnding warns that a lease is ending (InS > 0) or has ended (InS == 0).
type LeaseEnding struct {
	Type   string `json:"type"`
	Lease  string `json:"lease"`
	InS    int    `json:"in_s"`
	Reason string `json:"reason"`
}

// Renew asks the relay to extend a lease (client → relay).
type Renew struct {
	Type  string `json:"type"`
	Lease string `json:"lease"`
}

// GatewayStatus tells a client about the gateway (relay → client).
type GatewayStatus struct {
	Type   string `json:"type"`
	Bridge string `json:"bridge"` // BridgeOnline or BridgeOffline
	Node   *Node  `json:"node,omitempty"`
}

var pubkeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NormalizePubkey lowercases and trims a node pubkey, and reports whether it
// is well formed (64 hex chars, 32 bytes).
func NormalizePubkey(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	return s, pubkeyPattern.MatchString(s)
}

// --- Pairing (HTTP, under /mesh/relay/api/pair/) ----------------------------

// Pairing poll statuses (PairResult.Status).
const (
	PairPending  = "pending"
	PairApproved = "approved"
	PairDenied   = "denied"
	PairExpired  = "expired"
)

// ErrInvalidPassphrase is pair/start's 403 body code for a wrong, expired,
// used-up or revoked passphrase. It deliberately doesn't say which.
const ErrInvalidPassphrase = "invalid_passphrase"

// PairNode is the node a bridge reports when pairing: Node plus its link kind.
// State may be NodeDisconnected if no node has been found yet.
type PairNode struct {
	Node
	Link string `json:"link,omitempty"`
}

// PairStartRequest is the body of POST pair/start (no auth).
type PairStartRequest struct {
	Agent     Agent    `json:"agent"`
	HostLabel string   `json:"host_label,omitempty"`
	Node      PairNode `json:"node"`
	// Passphrase, when present, redeems a pre-approved invite.
	Passphrase string `json:"passphrase,omitempty"`
}

// PairStartResponse starts the device-code flow (pair/start without a
// passphrase).
type PairStartResponse struct {
	PairID          string `json:"pair_id"`
	PollSecret      string `json:"poll_secret"`
	UserCode        string `json:"user_code"` // XXX-XXX
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"` // seconds
	Interval        int    `json:"interval"`   // seconds between polls
}

// PairPollRequest is the body of POST pair/poll.
type PairPollRequest struct {
	PairID     string `json:"pair_id"`
	PollSecret string `json:"poll_secret"`
}

// PairResult answers pair/poll, and pair/start with a valid passphrase. An
// approved result carries the bridge token, delivered exactly once.
type PairResult struct {
	Status         string `json:"status"`
	Gateway        string `json:"gateway,omitempty"`
	Token          string `json:"token,omitempty"`
	Relay          string `json:"relay,omitempty"` // wss://…/mesh/relay
	ExpectedPubkey string `json:"expected_pubkey,omitempty"`
}

// PairError is the body of a pairing error response, e.g. 403
// {"error":"invalid_passphrase"}.
type PairError struct {
	Error string `json:"error"`
}

// NormalizePassphrase applies the spec's forgiving match: lowercase, with
// spaces, hyphens, commas and runs of them all treated as one separator.
// "Maple River quartz  otter" becomes "maple-river-quartz-otter".
func NormalizePassphrase(s string) string {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '-' || r == ',' || r == '\t' || r == '\n' || r == '\r'
	})
	return strings.Join(words, "-")
}
