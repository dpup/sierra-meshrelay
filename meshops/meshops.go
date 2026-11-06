// Package meshops runs MeshCore companion-protocol operations over any frame
// transport, such as a relay lease. It covers contacts, repeater login,
// status and telemetry, which is what a poller needs to read a remote
// repeater through a gateway's node.
//
// Frames are payloads only: no '<'/'>' header and no length. Session parses
// only the replies to the commands it sends and drops everything else
// unread. Nothing here logs a frame: the node also pushes private messages
// while a session holds it.
//
// The command and reply layouts follow examples/companion_radio/MyMesh.cpp
// and examples/simple_repeater in meshcore-dev/MeshCore.
package meshops

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Companion commands, replies and pushes used here.
const (
	cmdAppStart         byte = 1
	cmdGetContacts      byte = 4
	cmdGetDeviceTime    byte = 5
	cmdSetDeviceTime    byte = 6
	cmdAddUpdateContact byte = 9
	cmdRemoveContact    byte = 15
	cmdSendLogin        byte = 26
	cmdSendStatusReq    byte = 27
	cmdGetContactByKey  byte = 30
	cmdSendTelemetryReq byte = 39

	respOK            byte = 0
	respErr           byte = 1
	respContactsStart byte = 2
	respContact       byte = 3
	respEndOfContacts byte = 4
	respSelfInfo      byte = 5
	respSent          byte = 6
	respCurrTime      byte = 9

	pushLoginSuccess      byte = 0x85
	pushLoginFail         byte = 0x86
	pushStatusResponse    byte = 0x87
	pushTelemetryResponse byte = 0x8B
)

// Contact types (ADV_TYPE_*).
const (
	TypeChat     byte = 1
	TypeRepeater byte = 2
	TypeRoom     byte = 3
	TypeSensor   byte = 4
)

const (
	// PubkeySize is a node's public key length.
	PubkeySize  = 32
	maxPathSize = 64
	nameSize    = 32
	// outPathUnknown makes the node flood to a contact until it learns a path.
	outPathUnknown byte = 0xFF
	// contactFrameLen is RESP_CODE_CONTACT: code, pubkey, type, flags,
	// out_path_len, out_path, name, last_advert, lat, lon, lastmod.
	contactFrameLen = 1 + PubkeySize + 3 + maxPathSize + nameSize + 4 + 4 + 4 + 4
	selfInfoNameOff = 4 + PubkeySize + 4 + 4 + 1 + 1 + 1 + 1 + 4 + 4 + 1 + 1
	pushPrefixLen   = 6
)

// Errors. A node that answers RESP_CODE_ERR gives ErrRefused (with the
// firmware's error code); a reply that never comes gives ErrNoReply.
var (
	ErrRefused      = errors.New("the node refused the command")
	ErrNoReply      = errors.New("no reply")
	ErrNotFound     = errors.New("not in the node's contacts")
	ErrLoginFailed  = errors.New("the repeater refused the password")
	ErrNoMeshReply  = errors.New("no reply from the repeater over the mesh")
	ErrSessionEnded = errors.New("session ended")
)

// Firmware error codes (ERR_CODE_*), in a RefusedError.
const (
	ErrCodeUnsupported = 1
	ErrCodeNotFound    = 2
	ErrCodeTableFull   = 3
	ErrCodeBadState    = 4
)

// RefusedError is a RESP_CODE_ERR answer. It matches ErrRefused.
type RefusedError struct{ Code int }

func (e *RefusedError) Error() string {
	return fmt.Sprintf("the node refused the command (error %d)", e.Code)
}
func (e *RefusedError) Is(t error) bool { return t == ErrRefused }
func refused(f []byte) error {
	if len(f) > 1 {
		return &RefusedError{Code: int(f[1])}
	}
	return &RefusedError{}
}

// Session talks to one node. Feed it every frame from the node with Deliver;
// it sends through the function given to NewSession. One operation runs at a
// time (the firmware itself tracks one pending request).
type Session struct {
	send   func([]byte) error
	frames chan []byte
	done   chan struct{}

	// Timeout bounds a reply from the node itself (5s).
	Timeout time.Duration
	// MeshTimeout bounds a reply from a remote node over the mesh, when the
	// node gives no estimate. The node's estimate is doubled, then clamped
	// to [MinMeshTimeout, MaxMeshTimeout].
	MinMeshTimeout time.Duration // 8s
	MaxMeshTimeout time.Duration // 45s
	Now            func() time.Time
}

// NewSession returns a Session that sends frames with send.
func NewSession(send func([]byte) error) *Session {
	return &Session{send: send, frames: make(chan []byte, 256), done: make(chan struct{}),
		Timeout: 5 * time.Second, MinMeshTimeout: 8 * time.Second, MaxMeshTimeout: 45 * time.Second, Now: time.Now}
}

// Deliver hands the session one frame from the node. It never blocks: when
// the session is not reading (or has fallen far behind), the frame is
// dropped, which only ever loses pushes nobody asked for.
func (s *Session) Deliver(frame []byte) {
	select {
	case s.frames <- append([]byte(nil), frame...):
	default:
	}
}

// End makes pending and later operations fail with ErrSessionEnded (the
// lease is gone).
func (s *Session) End() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// drain discards frames left over from earlier operations.
func (s *Session) drain() {
	for {
		select {
		case <-s.frames:
		default:
			return
		}
	}
}

// next waits for a frame that match accepts, dropping the rest.
func (s *Session) next(ctx context.Context, timeout time.Duration, match func([]byte) bool) ([]byte, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case f := <-s.frames:
			if match(f) {
				return f, nil
			}
		case <-t.C:
			return nil, ErrNoReply
		case <-s.done:
			return nil, ErrSessionEnded
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// call sends frame and returns the first reply with one of codes, or ERR.
func (s *Session) call(ctx context.Context, frame []byte, codes ...byte) ([]byte, error) {
	s.drain()
	if err := s.send(frame); err != nil {
		return nil, err
	}
	f, err := s.next(ctx, s.Timeout, func(f []byte) bool {
		return len(f) > 0 && (f[0] == respErr || bytes.IndexByte(codes, f[0]) >= 0)
	})
	if err != nil {
		return nil, err
	}
	if f[0] == respErr {
		return nil, refused(f)
	}
	return f, nil
}

// SelfInfo is the node's own identity, from APP_START.
type SelfInfo struct {
	Pubkey string // 64 lowercase hex
	Name   string
}

// AppStart starts an app session on the node and returns its identity.
func (s *Session) AppStart(ctx context.Context, appName string) (SelfInfo, error) {
	f, err := s.call(ctx, append([]byte{cmdAppStart, 1, 0, 0, 0, 0, 0, 0}, appName...), respSelfInfo)
	if err != nil {
		return SelfInfo{}, err
	}
	if len(f) < 4+PubkeySize {
		return SelfInfo{}, fmt.Errorf("short SELF_INFO (%d bytes)", len(f))
	}
	si := SelfInfo{Pubkey: hex.EncodeToString(f[4 : 4+PubkeySize])}
	if len(f) > selfInfoNameOff {
		si.Name = cString(f[selfInfoNameOff:])
	}
	return si, nil
}

// DeviceTime reads the node's clock.
func (s *Session) DeviceTime(ctx context.Context) (time.Time, error) {
	f, err := s.call(ctx, []byte{cmdGetDeviceTime}, respCurrTime)
	if err != nil {
		return time.Time{}, err
	}
	if len(f) < 5 {
		return time.Time{}, errors.New("short CURR_TIME")
	}
	return time.Unix(int64(binary.LittleEndian.Uint32(f[1:5])), 0), nil
}

// SetDeviceTime sets the node's clock. Every companion app does this when it
// connects; a repeater rejects a login whose timestamp isn't newer than the
// last one it saw from this node, so a node whose clock went backwards (a
// reboot without a real-time clock) can't log in until it's set.
func (s *Session) SetDeviceTime(ctx context.Context, t time.Time) error {
	_, err := s.call(ctx, binary.LittleEndian.AppendUint32([]byte{cmdSetDeviceTime}, uint32(t.Unix())), respOK)
	return err
}

// SyncClock sets the node's clock if it is more than skew off, and reports
// whether it did.
func (s *Session) SyncClock(ctx context.Context, skew time.Duration) (bool, error) {
	t, err := s.DeviceTime(ctx)
	if err != nil {
		return false, err
	}
	d := s.Now().Sub(t)
	if d < 0 {
		d = -d
	}
	if d <= skew {
		return false, nil
	}
	return true, s.SetDeviceTime(ctx, s.Now())
}

// Contact is one entry in the node's contacts.
type Contact struct {
	Pubkey     string // 64 lowercase hex
	Type       byte   // TypeRepeater etc.
	Name       string
	LastAdvert time.Time // the contact's own advert timestamp; zero if never heard
	// PathKnown says the node has a direct route to the contact; without
	// one, it floods.
	PathKnown bool
}

func parseContact(f []byte) (Contact, error) {
	if len(f) < 1+PubkeySize+3+maxPathSize+nameSize+4 || f[0] != respContact {
		return Contact{}, fmt.Errorf("short or wrong CONTACT frame (%d bytes)", len(f))
	}
	i := 1
	c := Contact{Pubkey: hex.EncodeToString(f[i : i+PubkeySize])}
	i += PubkeySize
	c.Type = f[i]
	pathLen := f[i+2]
	c.PathKnown = pathLen != outPathUnknown
	i += 3 + maxPathSize
	c.Name = cString(f[i : i+nameSize])
	i += nameSize
	if ts := binary.LittleEndian.Uint32(f[i : i+4]); ts != 0 {
		c.LastAdvert = time.Unix(int64(ts), 0)
	}
	return c, nil
}

// Contacts lists the node's contacts.
func (s *Session) Contacts(ctx context.Context) ([]Contact, error) {
	if _, err := s.call(ctx, []byte{cmdGetContacts}, respContactsStart); err != nil {
		return nil, err
	}
	var out []Contact
	for {
		f, err := s.next(ctx, s.Timeout, func(f []byte) bool {
			return len(f) > 0 && (f[0] == respContact || f[0] == respEndOfContacts || f[0] == respErr)
		})
		if err != nil {
			return nil, err
		}
		switch f[0] {
		case respEndOfContacts:
			return out, nil
		case respErr:
			return nil, refused(f)
		}
		c, err := parseContact(f)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
}

// Contact looks up one contact by pubkey. It returns ErrNotFound if the node
// doesn't have it.
func (s *Session) Contact(ctx context.Context, pubkey string) (Contact, error) {
	pk, err := decodePubkey(pubkey)
	if err != nil {
		return Contact{}, err
	}
	f, err := s.call(ctx, append([]byte{cmdGetContactByKey}, pk...), respContact)
	var re *RefusedError
	if errors.As(err, &re) && re.Code == ErrCodeNotFound {
		return Contact{}, ErrNotFound
	}
	if err != nil {
		return Contact{}, err
	}
	return parseContact(f)
}

// AddContact adds a contact with no known path (the node floods to it until
// it learns one). It returns a RefusedError with ErrCodeTableFull when the
// node's contacts are full.
func (s *Session) AddContact(ctx context.Context, c Contact) error {
	pk, err := decodePubkey(c.Pubkey)
	if err != nil {
		return err
	}
	f := append([]byte{cmdAddUpdateContact}, pk...)
	f = append(f, c.Type, 0, outPathUnknown)
	f = append(f, make([]byte, maxPathSize)...)
	name := make([]byte, nameSize)
	copy(name[:nameSize-1], c.Name)
	f = append(f, name...)
	var ts uint32
	if !c.LastAdvert.IsZero() {
		ts = uint32(c.LastAdvert.Unix())
	}
	f = binary.LittleEndian.AppendUint32(f, ts)
	_, err = s.call(ctx, f, respOK)
	return err
}

// RemoveContact removes a contact.
func (s *Session) RemoveContact(ctx context.Context, pubkey string) error {
	pk, err := decodePubkey(pubkey)
	if err != nil {
		return err
	}
	_, err = s.call(ctx, append([]byte{cmdRemoveContact}, pk...), respOK)
	var re *RefusedError
	if errors.As(err, &re) && re.Code == ErrCodeNotFound {
		return ErrNotFound
	}
	return err
}

// meshRequest sends a request that travels over the mesh: the node answers
// RESP_CODE_SENT with an estimated timeout, and the remote node's answer
// arrives later as a push carrying the first 6 bytes of its pubkey.
func (s *Session) meshRequest(ctx context.Context, frame []byte, pk []byte, pushes ...byte) ([]byte, error) {
	sent, err := s.call(ctx, frame, respSent)
	var re *RefusedError
	if errors.As(err, &re) && re.Code == ErrCodeNotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	wait := s.MaxMeshTimeout
	if len(sent) >= 10 {
		if est := time.Duration(binary.LittleEndian.Uint32(sent[6:10])) * time.Millisecond; est > 0 {
			wait = 2 * est
		}
	}
	wait = min(max(wait, s.MinMeshTimeout), s.MaxMeshTimeout)
	f, err := s.next(ctx, wait, func(f []byte) bool {
		return len(f) >= 2+pushPrefixLen && bytes.IndexByte(pushes, f[0]) >= 0 && bytes.Equal(f[2:2+pushPrefixLen], pk[:pushPrefixLen])
	})
	if errors.Is(err, ErrNoReply) {
		return nil, ErrNoMeshReply
	}
	return f, err
}

// Login is a successful login to a repeater (or room server).
type Login struct {
	Admin bool
	// Permissions is the ACL role the repeater granted, when its firmware
	// says (PERM_ACL_*: 1 guest, 2 read-write, 3 admin); 0 if not reported.
	Permissions byte
}

// Access names the login's level: "admin" or "guest".
func (l Login) Access() string {
	if l.Admin || l.Permissions&0x03 == 3 {
		return "admin"
	}
	return "guest"
}

// Login logs in to a repeater in the node's contacts. The password is sent
// to the repeater encrypted, as every MeshCore app does; it never appears in
// an error. It returns ErrLoginFailed for a wrong password, ErrNoMeshReply if
// the repeater didn't answer, and ErrNotFound if it isn't a contact.
func (s *Session) Login(ctx context.Context, pubkey, password string) (Login, error) {
	pk, err := decodePubkey(pubkey)
	if err != nil {
		return Login{}, err
	}
	f := append([]byte{cmdSendLogin}, pk...)
	f, err = s.meshRequest(ctx, append(f, password...), pk, pushLoginSuccess, pushLoginFail)
	if err != nil {
		return Login{}, err
	}
	if f[0] == pushLoginFail {
		return Login{}, ErrLoginFailed
	}
	l := Login{Admin: f[1] != 0}
	if len(f) >= 2+pushPrefixLen+4+1 {
		l.Permissions = f[2+pushPrefixLen+4]
	}
	return l, nil
}

// Status is a repeater's status reply (struct RepeaterStats). Fields the
// repeater's firmware didn't send are nil, never zero.
type Status struct {
	BatteryMilliVolts *int     `json:"battery_mv"`
	TxQueueLen        *int     `json:"tx_queue_len"`
	NoiseFloorDBm     *int     `json:"noise_floor_dbm"`
	LastRSSIdBm       *int     `json:"last_rssi_dbm"`
	PacketsRecv       *int64   `json:"nb_recv"`
	PacketsSent       *int64   `json:"nb_sent"`
	AirtimeS          *int64   `json:"airtime_s"`
	UptimeS           *int64   `json:"uptime_s"`
	SentFlood         *int64   `json:"sent_flood"`
	SentDirect        *int64   `json:"sent_direct"`
	RecvFlood         *int64   `json:"recv_flood"`
	RecvDirect        *int64   `json:"recv_direct"`
	ErrEvents         *int     `json:"err_events"`
	LastSNRdB         *float64 `json:"last_snr_db"`
	DirectDups        *int     `json:"direct_dups"`
	FloodDups         *int     `json:"flood_dups"`
	RxAirtimeS        *int64   `json:"rx_airtime_s"`
	RecvErrors        *int64   `json:"recv_errors"`
}

// ParseStatus reads a RepeaterStats struct (little endian, packed).
// Exported for tests and other transports.
func ParseStatus(b []byte) Status {
	var s Status
	u16 := func(off int) *int {
		if len(b) < off+2 {
			return nil
		}
		v := int(binary.LittleEndian.Uint16(b[off:]))
		return &v
	}
	i16 := func(off int) *int {
		if len(b) < off+2 {
			return nil
		}
		v := int(int16(binary.LittleEndian.Uint16(b[off:])))
		return &v
	}
	u32 := func(off int) *int64 {
		if len(b) < off+4 {
			return nil
		}
		v := int64(binary.LittleEndian.Uint32(b[off:]))
		return &v
	}
	s.BatteryMilliVolts, s.TxQueueLen, s.NoiseFloorDBm, s.LastRSSIdBm = u16(0), u16(2), i16(4), i16(6)
	s.PacketsRecv, s.PacketsSent, s.AirtimeS, s.UptimeS = u32(8), u32(12), u32(16), u32(20)
	s.SentFlood, s.SentDirect, s.RecvFlood, s.RecvDirect = u32(24), u32(28), u32(32), u32(36)
	s.ErrEvents = u16(40)
	if snr := i16(42); snr != nil {
		v := float64(*snr) / 4
		s.LastSNRdB = &v
	}
	s.DirectDups, s.FloodDups = u16(44), u16(46)
	s.RxAirtimeS, s.RecvErrors = u32(48), u32(52)
	return s
}

// Status asks a repeater for its status. Guests may ask; log in first.
func (s *Session) Status(ctx context.Context, pubkey string) (Status, error) {
	pk, err := decodePubkey(pubkey)
	if err != nil {
		return Status{}, err
	}
	f, err := s.meshRequest(ctx, append([]byte{cmdSendStatusReq}, pk...), pk, pushStatusResponse)
	if err != nil {
		return Status{}, err
	}
	// code, reserved, 6-byte prefix, then the reply minus its 4-byte tag.
	return ParseStatus(f[2+pushPrefixLen:]), nil
}

// Telemetry is the part of a repeater's telemetry the poller reads: the
// node's own (channel 1) voltage and temperature. Unread values are nil.
type Telemetry struct {
	Voltage      *float64 `json:"voltage"`
	TemperatureC *float64 `json:"temperature_c"`
}

// Telemetry asks a repeater for its telemetry (Cayenne LPP). A guest gets
// only the base set, which includes voltage and the MCU temperature.
func (s *Session) Telemetry(ctx context.Context, pubkey string) (Telemetry, error) {
	pk, err := decodePubkey(pubkey)
	if err != nil {
		return Telemetry{}, err
	}
	f, err := s.meshRequest(ctx, append([]byte{cmdSendTelemetryReq, 0, 0, 0}, pk...), pk, pushTelemetryResponse)
	if err != nil {
		return Telemetry{}, err
	}
	return ParseLPP(f[2+pushPrefixLen:]), nil
}

// LPP data sizes by type (Cayenne LPP plus MeshCore's additions), to step
// over values the poller doesn't read.
var lppSize = map[byte]int{
	0: 1, 1: 1, 2: 2, 3: 2, 100: 4, 101: 2, 102: 1, 103: 2, 104: 1, 113: 6, 115: 2, 116: 2, 117: 2,
	118: 4, 120: 1, 121: 2, 125: 2, 128: 2, 130: 4, 131: 4, 132: 2, 133: 4, 134: 6, 135: 3, 136: 9, 142: 1,
}

const (
	lppTemperature byte = 103 // int16, 0.1 °C
	lppVoltage     byte = 116 // uint16, 0.01 V
	telemChanSelf  byte = 1
)

// ParseLPP reads channel 1's voltage and temperature from a Cayenne LPP
// buffer, stopping at the first type it can't size.
func ParseLPP(b []byte) Telemetry {
	var t Telemetry
	for len(b) >= 2 {
		ch, typ := b[0], b[1]
		n, ok := lppSize[typ]
		if !ok || len(b) < 2+n {
			break
		}
		v := b[2 : 2+n]
		if ch == telemChanSelf {
			switch typ {
			case lppVoltage:
				x := float64(binary.BigEndian.Uint16(v)) / 100
				t.Voltage = &x
			case lppTemperature:
				x := float64(int16(binary.BigEndian.Uint16(v))) / 10
				t.TemperatureC = &x
			}
		}
		b = b[2+n:]
	}
	return t
}

func decodePubkey(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(s)))
	if err != nil || len(b) != PubkeySize {
		return nil, fmt.Errorf("not a 64-hex-character pubkey")
	}
	return b, nil
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.ToValidUTF8(string(b), "")
}
