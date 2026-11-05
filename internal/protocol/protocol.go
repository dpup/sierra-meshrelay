// Package protocol holds the SIERRA Mesh Relay wire protocol v1 types and
// constants (docs/mesh-relay-wireprotocol.md).
//
// It is deliberately self-contained (no I/O, no dependencies) so it can move
// into the shared sierra-mesh-protocol module unchanged.
package protocol

import "encoding/json"

// Subprotocol names carry the protocol's major version.
const (
	BridgeSubprotocol = "sierra-mesh-bridge.v1"
	ClientSubprotocol = "sierra-mesh-client.v1"
	Version           = 1
)

// Size limits.
const (
	// MaxFrameBytes is the largest binary message (one companion frame payload).
	MaxFrameBytes = 512
	// MaxControlBytes is the largest text (JSON control) message the relay accepts.
	MaxControlBytes = 8 << 10
)

// WebSocket close codes.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseMessageTooBig   = 1009
	CloseBadToken        = 4001
	CloseGatewayMismatch = 4002
	CloseProtocolError   = 4003
	CloseUnsupported     = 4004
	CloseReplaced        = 4009
	CloseBusy            = 4029
)

// Node states.
const (
	NodeConnected    = "connected"
	NodeDisconnected = "disconnected"
	NodeMismatch     = "mismatch"
	NodePaused       = "paused"
)

// Link kinds.
const (
	LinkSerial = "serial"
	LinkTCP    = "tcp"
)

// Error codes. Never depend on an error's Message, only its Code.
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

// Message types.
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

// Agent identifies the bridge software.
type Agent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Link describes how the bridge reaches the node.
type Link struct {
	Kind   string `json:"kind,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Node is hello.node and the body of node_status.
type Node struct {
	State  string `json:"state"`
	Pubkey string `json:"pubkey,omitempty"`
	Name   string `json:"name,omitempty"`
	FW     string `json:"fw,omitempty"`
	Model  string `json:"model,omitempty"`
	Until  string `json:"until,omitempty"`
	Error  string `json:"error,omitempty"`
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

// Welcome is the relay's reply to hello.
type Welcome struct {
	Type          string `json:"type"`
	Session       string `json:"session"`
	PingIntervalS int    `json:"ping_interval_s"`
	ServerTime    string `json:"server_time"`
}

// NodeStatus reports a node change. Fields are hello.node, flattened.
type NodeStatus struct {
	Type string `json:"type"`
	Node
}

// Stats is the bridge's periodic counters message.
type Stats struct {
	Type               string `json:"type"`
	UptimeS            int64  `json:"uptime_s"`
	NodeLinkReconnects int64  `json:"node_link_reconnects"`
	FramesToNode       int64  `json:"frames_to_node"`
	FramesFromNode     int64  `json:"frames_from_node"`
	RelayReconnects    int64  `json:"relay_reconnects"`
}

// Attach tells the bridge a client took a lease.
type Attach struct {
	Type   string `json:"type"`
	Lease  string `json:"lease"`
	Client string `json:"client"`
}

// Detach tells the bridge a lease ended.
type Detach struct {
	Type  string `json:"type"`
	Lease string `json:"lease"`
}

// Relink asks the bridge to reopen its node link.
type Relink struct {
	Type string `json:"type"`
}

// Error is an error report from either side.
type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Lease is sent to a client when it gets (or renews) a lease.
type Lease struct {
	Type      string `json:"type"`
	Lease     string `json:"lease"`
	Gateway   string `json:"gateway"`
	ExpiresAt string `json:"expires_at"`
	Node      *Node  `json:"node,omitempty"`
}

// LeaseEnding warns a client its lease is ending (InS > 0) or has ended (InS == 0).
type LeaseEnding struct {
	Type   string `json:"type"`
	Lease  string `json:"lease"`
	InS    int    `json:"in_s"`
	Reason string `json:"reason"`
}

// Renew asks the relay to extend a lease.
type Renew struct {
	Type  string `json:"type"`
	Lease string `json:"lease"`
}

// GatewayStatus tells a client about the gateway's state.
type GatewayStatus struct {
	Type   string `json:"type"`
	Bridge string `json:"bridge"`
	Node   *Node  `json:"node,omitempty"`
}

// TypeOf returns a control message's type, or "" if it isn't a JSON object
// with a string type field.
func TypeOf(data []byte) string {
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &env) != nil {
		return ""
	}
	return env.Type
}
