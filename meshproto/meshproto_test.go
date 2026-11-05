package meshproto

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// messageTypes maps every control message type in the spec to its Go type.
var messageTypes = map[string]any{
	TypeHello:           Hello{},
	TypeWelcome:         Welcome{},
	TypeNodeStatus:      NodeStatus{},
	TypeStats:           StatsMsg{},
	TypeAttach:          Attach{},
	TypeDetach:          Detach{},
	TypeRelink:          Relink{},
	TypeUpdateAvailable: UpdateAvailable{},
	TypeError:           Error{},
	TypeLease:           Lease{},
	TypeLeaseEnding:     LeaseEnding{},
	TypeRenew:           Renew{},
	TypeGatewayStatus:   GatewayStatus{},
}

var jsonBlock = regexp.MustCompile("(?s)```json\n(.*?)```")

// TestSpecExamples decodes every typed JSON example in PROTOCOL.md into its Go
// type, rejecting unknown fields, and checks that a round trip keeps every
// field. A field added to the spec but not the types (or misspelled in
// either) fails here.
func TestSpecExamples(t *testing.T) {
	spec, err := os.ReadFile("../PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range jsonBlock.FindAllSubmatch(spec, -1) {
		dec := json.NewDecoder(bytes.NewReader(m[1]))
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("spec example isn't valid JSON: %v\n%s", err, m[1])
			}
			typ := TypeOf(raw)
			if typ == "" {
				continue // HTTP bodies (pairing, tickets) have no type
			}
			proto, ok := messageTypes[typ]
			if !ok {
				t.Errorf("spec example has type %q with no Go type", typ)
				continue
			}
			seen[typ] = true
			v := reflect.New(reflect.TypeOf(proto)).Interface()
			strict := json.NewDecoder(bytes.NewReader(raw))
			strict.DisallowUnknownFields()
			if err := strict.Decode(v); err != nil {
				t.Errorf("%s example doesn't fit %T: %v\n%s", typ, proto, err, raw)
				continue
			}
			out, _ := json.Marshal(v)
			var want, got map[string]any
			_ = json.Unmarshal(raw, &want)
			_ = json.Unmarshal(out, &got)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s doesn't round trip:\n spec: %s\n  got: %s", typ, raw, out)
			}
		}
	}
	for typ := range messageTypes {
		if !seen[typ] {
			t.Errorf("no spec example for %q", typ)
		}
	}
}

// TestSpecMentionsConstants checks every code and reason constant appears in
// the spec, so nothing is defined here that the spec doesn't describe.
func TestSpecMentionsConstants(t *testing.T) {
	spec, err := os.ReadFile("../PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(spec)
	for _, c := range []string{
		BridgeSubprotocol, ClientSubprotocol,
		ErrHelloRequired, ErrUnsupportedProtocol, ErrFrameTooLarge, ErrBusy, ErrGatewayOffline,
		ErrPubkeyMismatch, ErrPubkeyUnverified, ErrRenewDenied, ErrUnknownLease, NodeStateErr(NodePaused),
		ReasonExpiry, ReasonPreempted, ReasonGatewayOffline, ReasonIdle, ReasonRevoked,
		ReasonPubkeyMismatch, ReasonRelayShutdown, ReasonHostPaused,
		NodeConnected, NodeDisconnected, NodeMismatch, NodePaused,
	} {
		if !strings.Contains(s, "`"+c+"`") {
			t.Errorf("%q isn't in PROTOCOL.md", c)
		}
	}
	for _, code := range []string{"1000", "1001", "1009", "4001", "4002", "4003", "4004", "4009", "4029"} {
		if !strings.Contains(s, "| "+code+" |") {
			t.Errorf("close code %s isn't in the close-code table", code)
		}
	}
}

func TestOmitEmpty(t *testing.T) {
	b, _ := json.Marshal(NodeStatus{Type: TypeNodeStatus, Node: Node{State: NodeDisconnected}})
	if string(b) != `{"type":"node_status","state":"disconnected"}` {
		t.Fatalf("got %s", b)
	}
}

func TestNormalizePubkey(t *testing.T) {
	k := strings.Repeat("Ab", 32)
	if got, ok := NormalizePubkey("  " + k + "\n"); !ok || got != strings.ToLower(k) {
		t.Fatalf("got %q %v", got, ok)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 64), strings.Repeat("a", 66)} {
		if _, ok := NormalizePubkey(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTypeOf(t *testing.T) {
	for in, want := range map[string]string{
		`{"type":"hello"}`: "hello", `{"type":3}`: "", `[]`: "", `nope`: "", `{}`: "",
	} {
		if got := TypeOf([]byte(in)); got != want {
			t.Errorf("TypeOf(%s) = %q, want %q", in, got, want)
		}
	}
}
