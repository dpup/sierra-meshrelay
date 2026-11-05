package companion

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Companion command and response codes (examples/companion_radio/MyMesh.cpp).
const (
	CmdAppStart       byte = 0x01
	CmdDeviceQuery    byte = 0x16 // 22
	RespSelfInfo      byte = 0x05
	RespDeviceInfo    byte = 0x0d // 13
	PubkeySize             = 32
	AppName                = "sierra-meshbridge"
	deviceQueryAppVer byte = 3
)

// SELF_INFO layout: code, adv_type, tx_power, max_tx_power, pubkey[32],
// lat i32, lon i32, multi_acks, advert_loc_policy, telemetry_modes,
// manual_add_contacts, freq u32, bw u32, sf, cr, name (rest of frame).
const (
	selfInfoPubkeyOff = 4
	selfInfoNameOff   = selfInfoPubkeyOff + PubkeySize + 4 + 4 + 1 + 1 + 1 + 1 + 4 + 4 + 1 + 1 // 58
)

// DEVICE_INFO layout: code, fw_ver_code, max_contacts/2, max_channels,
// ble_pin u32, build_date[12], manufacturer[40], fw_version[20], ...
// The BLE PIN is in here: never log this frame.
const (
	deviceInfoModelOff = 1 + 1 + 1 + 1 + 4 + 12 // 20
	deviceInfoModelLen = 40
	deviceInfoFWOff    = deviceInfoModelOff + deviceInfoModelLen // 60
	deviceInfoFWLen    = 20
)

// Identity is what the bridge learns about a node at link-up.
type Identity struct {
	Pubkey string // lowercase hex, 64 chars
	Name   string
	FW     string // e.g. "v1.18.0"; empty if DEVICE_QUERY wasn't answered
	Model  string // manufacturer/board name; empty if unknown
}

// AppStartFrame is the APP_START payload: code, app_ver, 6 reserved, name.
func AppStartFrame() []byte {
	return append([]byte{CmdAppStart, 1, 0, 0, 0, 0, 0, 0}, AppName...)
}

// DeviceQueryFrame is the DEVICE_QUERY payload: code, app_target_ver.
func DeviceQueryFrame() []byte { return []byte{CmdDeviceQuery, deviceQueryAppVer} }

// ParseSelfInfo extracts the pubkey and name from a SELF_INFO payload.
func ParseSelfInfo(f []byte) (pubkey, name string, err error) {
	if len(f) < selfInfoPubkeyOff+PubkeySize || f[0] != RespSelfInfo {
		return "", "", fmt.Errorf("companion: not a SELF_INFO frame (%d bytes)", len(f))
	}
	pubkey = hex.EncodeToString(f[selfInfoPubkeyOff : selfInfoPubkeyOff+PubkeySize])
	if len(f) > selfInfoNameOff {
		name = cString(f[selfInfoNameOff:])
	}
	return pubkey, name, nil
}

// ParseDeviceInfo extracts the firmware version and model from DEVICE_INFO.
func ParseDeviceInfo(f []byte) (fw, model string, err error) {
	if len(f) < 1 || f[0] != RespDeviceInfo {
		return "", "", errors.New("companion: not a DEVICE_INFO frame")
	}
	if len(f) >= deviceInfoModelOff+deviceInfoModelLen {
		model = cString(f[deviceInfoModelOff : deviceInfoModelOff+deviceInfoModelLen])
	}
	if len(f) >= deviceInfoFWOff+deviceInfoFWLen {
		fw = cString(f[deviceInfoFWOff : deviceInfoFWOff+deviceInfoFWLen])
	}
	return fw, model, nil
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(bytes.ToValidUTF8(b, nil))
}

// Identify runs the identity check: APP_START → SELF_INFO, then a best-effort
// DEVICE_QUERY → DEVICE_INFO for firmware and model. frames delivers payloads
// read from the node and is closed when the link's reader stops. Other frames
// (pushes, leftovers from a previous session) are discarded.
func Identify(ctx context.Context, frames <-chan []byte, write func([]byte) error, timeout time.Duration) (Identity, error) {
	var id Identity
	if err := write(AppStartFrame()); err != nil {
		return id, fmt.Errorf("writing APP_START: %w", err)
	}
	f, err := awaitFrame(ctx, frames, RespSelfInfo, timeout)
	if err != nil {
		return id, fmt.Errorf("waiting for SELF_INFO: %w", err)
	}
	if id.Pubkey, id.Name, err = ParseSelfInfo(f); err != nil {
		return id, err
	}
	// Firmware and model are nice to have; a node that doesn't answer
	// DEVICE_QUERY still passes the identity check.
	if err := write(DeviceQueryFrame()); err != nil {
		return id, fmt.Errorf("writing DEVICE_QUERY: %w", err)
	}
	if f, err := awaitFrame(ctx, frames, RespDeviceInfo, timeout); err == nil {
		id.FW, id.Model, _ = ParseDeviceInfo(f)
	} else if !errors.Is(err, ErrTimeout) {
		return id, fmt.Errorf("waiting for DEVICE_INFO: %w", err)
	}
	return id, nil
}

// ErrTimeout means the node didn't answer in time.
var ErrTimeout = errors.New("timed out")

// ErrLinkClosed means the node link closed during the identity check.
var ErrLinkClosed = errors.New("node link closed")

func awaitFrame(ctx context.Context, frames <-chan []byte, code byte, timeout time.Duration) ([]byte, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return nil, ErrLinkClosed
			}
			if len(f) > 0 && f[0] == code {
				return f, nil
			}
		case <-deadline.C:
			return nil, ErrTimeout
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
