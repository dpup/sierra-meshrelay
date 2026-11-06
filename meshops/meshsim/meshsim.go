// Package meshsim is a fake MeshCore companion node with repeaters in radio
// range, for testing code that uses meshops: contacts, repeater login,
// status, telemetry and the device clock. It answers frames like
// examples/companion_radio does, with the repeaters behaving like
// examples/simple_repeater.
package meshsim

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"
)

// Repeater is a repeater in the fake node's radio range.
type Repeater struct {
	Pubkey        string // 64 hex
	Name          string
	AdminPassword string
	GuestPassword string
	// Unreachable repeaters never answer over the mesh.
	Unreachable bool
	BatteryMV   uint16
	UptimeS     uint32
	TempC       float64
}

// Node is a fake companion node. The zero value is not usable; use New.
type Node struct {
	mu        sync.Mutex
	contacts  []contact
	repeaters map[string]*Repeater
	clock     time.Duration // offset from the real clock
	lastLogin map[string]uint32

	// MaxContacts is the contacts table size (default 100).
	MaxContacts int
	// MeshDelay is how long a mesh reply takes (default 20ms).
	MeshDelay time.Duration

	Logins, StatusReqs, TelemetryReqs int
}

type contact struct {
	pubkey []byte
	typ    byte
	name   string
	advert uint32
	path   byte
}

// New returns a node that has heard the given repeaters' adverts (they are in
// its contacts) and can reach them.
func New(heard ...*Repeater) *Node {
	n := &Node{repeaters: map[string]*Repeater{}, lastLogin: map[string]uint32{}, MaxContacts: 100,
		MeshDelay: 20 * time.Millisecond}
	for _, r := range heard {
		n.AddInRange(r)
		pk, _ := hex.DecodeString(r.Pubkey)
		n.contacts = append(n.contacts, contact{pubkey: pk, typ: 2, name: r.Name, advert: uint32(time.Now().Unix()), path: 0xFF})
	}
	return n
}

// AddInRange puts a repeater in range without the node having heard it.
func (n *Node) AddInRange(r *Repeater) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.repeaters[r.Pubkey] = r
}

// SetUnreachable makes a repeater in range stop (or start) answering.
func (n *Node) SetUnreachable(pubkey string, v bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if r := n.repeaters[pubkey]; r != nil {
		r.Unreachable = v
	}
}

// HasContact reports whether pubkey is in the node's contacts.
func (n *Node) HasContact(pubkey string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.find(pubkey) >= 0
}

// SetClockOffset moves the node's clock (negative: behind).
func (n *Node) SetClockOffset(d time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.clock = d
}

// ClockOffset is how far the node's clock is from the real one.
func (n *Node) ClockOffset() time.Duration {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.clock
}

func (n *Node) now() uint32 { return uint32(time.Now().Add(n.clock).Unix()) }

func (n *Node) find(pubkey string) int {
	pk, _ := hex.DecodeString(pubkey)
	for i, c := range n.contacts {
		if bytes.Equal(c.pubkey, pk) {
			return i
		}
	}
	return -1
}

// Handle answers one frame. reply may be called later, from another
// goroutine, for answers that come over the mesh. It returns false for
// commands it doesn't simulate.
func (n *Node) Handle(f []byte, reply func([]byte)) bool {
	if len(f) == 0 {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	pkAt := func(off int) (string, bool) {
		if len(f) < off+32 {
			return "", false
		}
		return hex.EncodeToString(f[off : off+32]), true
	}
	errFrame := func(code byte) { reply([]byte{1, code}) }
	switch f[0] {
	case 4: // GET_CONTACTS
		reply(binary.LittleEndian.AppendUint32([]byte{2}, uint32(len(n.contacts))))
		for _, c := range n.contacts {
			reply(contactFrame(c))
		}
		reply(binary.LittleEndian.AppendUint32([]byte{4}, n.now()))
	case 5: // GET_DEVICE_TIME
		reply(binary.LittleEndian.AppendUint32([]byte{9}, n.now()))
	case 6: // SET_DEVICE_TIME
		if len(f) < 5 {
			errFrame(4)
			return true
		}
		n.clock = time.Until(time.Unix(int64(binary.LittleEndian.Uint32(f[1:5])), 0)).Round(time.Second)
		reply([]byte{0})
	case 9: // ADD_UPDATE_CONTACT
		pk, ok := pkAt(1)
		if !ok || len(f) < 1+32+3+64+32 {
			errFrame(4)
			return true
		}
		c := contact{pubkey: f[1:33], typ: f[33], path: f[35], name: cstr(f[100:132])}
		if i := n.find(pk); i >= 0 {
			n.contacts[i] = c
		} else if len(n.contacts) >= n.MaxContacts {
			errFrame(3)
			return true
		} else {
			n.contacts = append(n.contacts, c)
		}
		reply([]byte{0})
	case 15: // REMOVE_CONTACT
		pk, _ := pkAt(1)
		i := n.find(pk)
		if i < 0 {
			errFrame(2)
			return true
		}
		n.contacts = append(n.contacts[:i], n.contacts[i+1:]...)
		reply([]byte{0})
	case 30: // GET_CONTACT_BY_KEY
		pk, _ := pkAt(1)
		i := n.find(pk)
		if i < 0 {
			errFrame(2)
			return true
		}
		reply(contactFrame(n.contacts[i]))
	case 26, 27, 39: // SEND_LOGIN, SEND_STATUS_REQ, SEND_TELEMETRY_REQ
		off := 1
		if f[0] == 39 {
			off = 4
		}
		pk, ok := pkAt(off)
		if !ok || n.find(pk) < 0 {
			errFrame(2)
			return true
		}
		reply(binary.LittleEndian.AppendUint32(append([]byte{6, 1}, 1, 2, 3, 4), 500))
		r := n.repeaters[pk]
		if r == nil || r.Unreachable {
			return true
		}
		prefix := f[off : off+6]
		var push []byte
		switch f[0] {
		case 26:
			n.Logins++
			pw := string(f[33:])
			ts := n.now()
			switch {
			case ts <= n.lastLogin[pk]: // replay protection: no answer
				return true
			case pw == r.AdminPassword && pw != "":
				push = append(append([]byte{0x85, 1}, prefix...), 0, 0, 0, 0, 3, 1)
			case pw == r.GuestPassword:
				push = append(append([]byte{0x85, 0}, prefix...), 0, 0, 0, 0, 1, 1)
			default:
				push = append([]byte{0x86, 0}, prefix...)
			}
			if push[0] == 0x85 {
				n.lastLogin[pk] = ts
			}
		case 27:
			n.StatusReqs++
			push = append(append([]byte{0x87, 0}, prefix...), r.stats()...)
		case 39:
			n.TelemetryReqs++
			push = append(append([]byte{0x8B, 0}, prefix...), r.lpp()...)
		}
		delay := n.MeshDelay
		time.AfterFunc(delay, func() { reply(push) })
	default:
		return false
	}
	return true
}

func (r *Repeater) stats() []byte {
	b := make([]byte, 56)
	binary.LittleEndian.PutUint16(b[0:], r.BatteryMV)
	binary.LittleEndian.PutUint16(b[2:], 2)
	binary.LittleEndian.PutUint16(b[4:], uint16(0xFFFF-110+1)) // -110 dBm
	binary.LittleEndian.PutUint16(b[6:], uint16(0xFFFF-80+1))  // -80 dBm
	binary.LittleEndian.PutUint32(b[8:], 1000)
	binary.LittleEndian.PutUint32(b[12:], 900)
	binary.LittleEndian.PutUint32(b[16:], 120)
	binary.LittleEndian.PutUint32(b[20:], r.UptimeS)
	binary.LittleEndian.PutUint16(b[42:], 22) // SNR 5.5 dB
	return b
}

func (r *Repeater) lpp() []byte {
	v := uint16(float64(r.BatteryMV) / 10)
	t := uint16(int16(r.TempC * 10))
	return []byte{1, 116, byte(v >> 8), byte(v), 1, 103, byte(t >> 8), byte(t)}
}

func contactFrame(c contact) []byte {
	f := append([]byte{3}, c.pubkey...)
	f = append(f, c.typ, 0, c.path)
	f = append(f, make([]byte, 64)...)
	name := make([]byte, 32)
	copy(name[:31], c.name)
	f = append(f, name...)
	f = binary.LittleEndian.AppendUint32(f, c.advert)
	return append(f, make([]byte, 12)...)
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
