# meshbridge

Connects a MeshCore companion radio to the SIERRA Mesh Relay, so authorized SIERRA clients can use the radio remotely. The bridge dials **out** to the relay over WebSocket; nothing connects in to the host's network.

```
 node <--TCP/serial--> meshbridge ==WSS (dials out)==> relay <==WSS== client
```

- [docs/mesh-bridge-handoff.md](docs/mesh-bridge-handoff.md): what we're building and why, plus the milestones.
- [docs/mesh-relay-wireprotocol.md](docs/mesh-relay-wireprotocol.md): the wire protocol. It's shared with the relay (`dpup/sierra-data`), so change it in both places.

## Status

M1 is done: `meshbridge run` with a TCP (WiFi) node link, the full bridge side of the relay protocol, and the identity check. It's verified with meshcore-cli against a Heltec V3 through the production relay. The `connect` client shim (M6) is built too. Serial, pairing, the service, and the control API come in later milestones.

## Running

```sh
make build
cp examples/meshbridge.toml meshbridge.local.toml   # set gateway and node.tcp_host
MESHBRIDGE_TOKEN=smb_… ./bin/meshbridge run -config meshbridge.local.toml
```

`./bin/meshbridge find-node -host 192.168.1.50` runs the identity check against a TCP node and prints its name, pubkey, model and firmware. Use that pubkey as `node.expected_pubkey`.

### Using a remote gateway (client mode)

```sh
MESHBRIDGE_CLIENT_TOKEN=smc_… ./bin/meshbridge connect bridge-dev
meshcore-cli -t 127.0.0.1 -p 5000 infos
```

`connect` listens on `127.0.0.1:5000` and takes a lease only while an app is connected. It serves one app at a time, renews the lease automatically while the app is active, and explains any refusal in plain words (for example busy, offline or paused). Use `-listen` to choose a different port. Listening on a non-loopback address prints a warning, because companion apps have no authentication.

Without a radio, run `./bin/fakenode -listen 127.0.0.1:5000` and point `tcp_host` at it. The fake node answers `APP_START` and `DEVICE_QUERY`, and echoes every other frame back with a `0x80` prefix.

## Testing

| Command | What it runs | Needs |
|---|---|---|
| `make test` | Unit and integration tests with a fake node and a scripted relay (`internal/relaytest`) | Nothing |
| `make test-live` | The real bridge plus a fake node against the **production** relay on `bridge-dev-sim`: lease, `APP_START`/`SELF_INFO`, round trips, a 300-frame burst through backpressure, renew, busy (4029), and the full chain through `connect` | `.envrc` with `RELAY`, `SIM_BRIDGE_TOKEN`, `CLIENT_TOKEN` |
| ↳ admin checks | Relink via the admin API | `ADMIN` too; these skip without it |

Live-test rules:
- `bridge-dev` is reserved for real hardware. The relay pins a gateway to the first node pubkey it sees, so fake nodes stay on `bridge-dev-sim`.
- Don't run two bridges with the same token. They replace each other (4009).
- Never log frame payloads. They can carry private messages and channel keys.

## Layout

| Path | |
|---|---|
| `cmd/meshbridge` | The bridge CLI |
| `cmd/fakenode` | A standalone fake companion node |
| `internal/bridge` | Node manager, relay session, reconnect backoff |
| `internal/companion` | Companion framing and the identity check (verified against MeshCore companion-v1.18.0) |
| `internal/nodelink` | Node link interface and TCP link |
| `internal/protocol` | Wire-protocol types and constants, kept dependency-free so they can move into a shared module |
| `internal/relayclient` | Client side of the relay protocol |
| `internal/connect` | `meshbridge connect`: local TCP companion port ↔ relay lease |
| `internal/relaytest`, `internal/fakenode` | Test doubles |
| `e2e` | Live relay tests |
