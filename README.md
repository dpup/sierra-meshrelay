# sierra-meshrelay

Remote access to MeshCore companion radios for SIERRA. A volunteer's radio connects to the **relay** through **meshbridge**, and authorized clients reach the radio through the relay. The bridge dials **out** over WebSocket, so nothing ever connects in to a host's network.

This repo holds all three parts: the wire protocol (`PROTOCOL.md` and the Go types in `meshproto/`), the relay (`relay/`), and the bridge (`cmd/meshbridge`). A protocol change, the relay, the bridge and their end-to-end tests all land in one pull request.

## How it fits together

The same `meshbridge` binary runs at both ends, with the relay in the middle:

```
 host site (e.g. a cabin)              cloud                         your laptop
 ┌──────────────────────────┐                                   ┌────────────────────────────┐
 │ node ◄─TCP:5000/USB─┐    │                                   │   ┌─► meshbridge connect     │
 │                     │    │       data.sierragridteam.org     │   │   (127.0.0.1:5000)       │
 │   meshbridge run ───┘    │ ══WSS out══►  relay  ◄══WSS out══ │   │        ▲                 │
 │   (smb_ bridge token)    │                                   │   │   meshcore-cli -t 127…   │
 └──────────────────────────┘                                   └────────────────────────────┘
```

- **`meshbridge run`** runs at the host site, on any always-on computer that can reach the node. It holds the node's single companion connection and dials out to the relay with the gateway's bridge token (`smb_…`).
- **`meshbridge connect <gateway>`** runs on your machine. It makes the remote node look like a local radio on `127.0.0.1:5000` for meshcore-cli and other TCP companion apps. It uses your client token (`smc_…`) and holds a lease only while an app is connected.
- **The relay** authenticates both sides, pins each gateway to its node's pubkey, and gives one client at a time a lease on a gateway. It forwards frames without reading them.

Apps built on meshcore-ts will be able to talk to the relay directly through a planned relay transport, with no `connect` shim.

### Compared to Tailscale

The shape is similar. Both ends dial out to a service in the cloud, so neither side opens a port, changes its router, or needs a public IP, and it works behind CGNAT. `connect` plays the role of reaching a device on your tailnet as if it were local.

The differences:
- **Every frame goes through the relay.** Tailscale tries to set up a direct WireGuard connection between peers and only falls back to its DERP relays. Here, all traffic goes through the relay, like DERP-only mode. MeshCore traffic is a handful of small frames per second, so the extra hop doesn't matter, and the relay sits where access control and auditing belong.
- **Access is to one device's companion session, not a network.** A client gets an exclusive, time-limited lease on one gateway's node, not IP access to the host's LAN. The bridge only ever talks to the node.
- **Trust is anchored in the node.** Tailscale trusts device keys. Here, the relay pins each gateway to its node's MeshCore pubkey, so a swapped or reflashed radio is blocked until an admin accepts its key.

## Docs

- [PROTOCOL.md](PROTOCOL.md): the wire protocol. It's the canonical spec, and the Go types in `meshproto/` are tested against its examples.
- [docs/linux.md](docs/linux.md): run a long-lived bridge on Linux as a systemd service (download, setup, install, status, upgrade).
- [docs/relay.md](docs/relay.md): the relay's service spec, design decisions and deployment requirements.
- [docs/mesh-bridge-handoff.md](docs/mesh-bridge-handoff.md): the bridge's design and milestones.

### Where the relay runs

`relay/` is a library. The deployed relay is `cmd/mesh-relay` in [dpup/sierra-data](https://github.com/dpup/sierra-data), which imports this module and adds sierra-data's prefab config, its deploy handoff (passed in as `relay.Config.Gate`), the Docker image and the ECS deploy. To ship a relay change, merge it here, bump `github.com/dpup/sierra-meshrelay` in sierra-data, and deploy from there.

## Status

Relay: live at `data.sierragridteam.org`. It handles bridge and client sessions, leases, pubkey pinning, the admin API and public status. Pairing (device codes and passphrase invites) and `paused` are built and tested but not yet deployed.

Bridge: `setup` (pairing), `status`, `pause` and `resume` work over a loopback control API. M1 is done: `meshbridge run` with a TCP (WiFi) node link, the full bridge side of the relay protocol, and the identity check. It's verified with meshcore-cli against a Heltec V3 through the production relay. The `connect` client shim (M6) is built too. Serial, pairing, the service, and the control API come in later milestones.

## Running

On a Linux box, follow [docs/linux.md](docs/linux.md): download a release, then `sudo meshbridge setup && sudo meshbridge install`.

From source:

```sh
make build
./bin/meshbridge setup -config meshbridge.local.toml          # finds the node on the LAN; pairs by passphrase or code
./bin/meshbridge run -config meshbridge.local.toml
```

`setup` asks for the passphrase your SIERRA contact sent. If you don't have one, press Enter to get a pairing code to text them instead. When the code is approved it writes the config, including the token and the node's expected pubkey, readable only by you. To configure by hand instead, copy `examples/meshbridge.toml` and set `MESHBRIDGE_TOKEN`.

While it runs:

```sh
./bin/meshbridge status   -config meshbridge.local.toml
./bin/meshbridge pause    -config meshbridge.local.toml 1h      # or: tomorrow, until-resumed
./bin/meshbridge resume   -config meshbridge.local.toml
```

These talk to the running bridge's control API on `127.0.0.1:8737`, authenticated by the `control.token` file next to the config. Pausing frees the node's one connection for your own MeshCore app. A pause with no end time (`until-resumed`) survives a restart. A timed pause doesn't.

`./bin/meshbridge find-node -host 192.168.1.50` runs the identity check against a TCP node and prints its name, pubkey, model and firmware. Use that pubkey as `node.expected_pubkey`.

### Using a remote gateway (client mode)

```sh
MESHBRIDGE_CLIENT_TOKEN=smc_… ./bin/meshbridge connect bridge-dev
meshcore-cli -t 127.0.0.1 -p 5000 infos
```

`connect` listens on `127.0.0.1:5000` and takes a lease only while an app is connected. It serves one app at a time, renews the lease automatically while the app is active, and explains any refusal in plain words (for example busy, offline or paused). Use `-listen` to choose a different port. Listening on a non-loopback address prints a warning, because companion apps have no authentication.

### A local relay

`go run ./cmd/devrelay -gateway cabin` runs the real relay on `127.0.0.1:8090`, with a throwaway database, and prints an admin token, the gateway's bridge token, an operator token and ready-to-paste commands. Use it to try a bridge, `connect`, pairing or pause without the production relay. To pair against it, run `meshbridge setup -relay ws://127.0.0.1:8090/mesh/relay`, then approve the code with `curl -X POST -H "Authorization: Bearer <admin>" -d '{"gateway":"cabin2"}' http://127.0.0.1:8090/mesh/relay/api/pairings/<code>/approve`.

Without a radio, run `./bin/fakenode -listen 127.0.0.1:5000` and point `tcp_host` at it. The fake node answers `APP_START` and `DEVICE_QUERY`, and echoes every other frame back with a `0x80` prefix.

## Testing

| Command | What it runs | Needs |
|---|---|---|
| `make test` | Every package's tests, plus `e2e/local_test.go` and `e2e/host_test.go`: the **real relay and real bridge in one process**, with a fake node and a TCP client through `connect`. They cover the full chain, a burst through the relay's throttling, pubkey pinning and accept-pubkey, relink, busy (4029), a bridge dropping mid-lease, pause (the lease ends, `node_paused` refusals, resume), and pairing (device code, exactly-once delivery, wrong poll secret, deny, re-pair, passphrase invites with sloppy typing, multi-use naming, node-bound invites, revocation, rate limits). | Nothing |
| `make test-live` | The real bridge plus a fake node against the **production** relay on `bridge-dev-sim`: lease, `APP_START`/`SELF_INFO`, round trips, a 300-frame burst through backpressure, renew, busy (4029), and the full chain through `connect` | `.envrc` with `RELAY`, `SIM_BRIDGE_TOKEN`, `CLIENT_TOKEN` |
| ↳ admin checks | Relink via the admin API | `ADMIN` too; these skip without it |

Live-test rules:
- `bridge-dev` is reserved for real hardware. The relay pins a gateway to the first node pubkey it sees, so fake nodes stay on `bridge-dev-sim`.
- Don't run two bridges with the same token. They replace each other (4009).
- Never log frame payloads. They can carry private messages and channel keys.

## Layout

| Path | |
|---|---|
| `PROTOCOL.md`, `meshproto/` | The wire protocol spec and its Go types |
| `relay/` | The relay: bridge and client sessions, leases, pinning, SQLite store, admin API |
| `cmd/meshbridge` | The bridge CLI (`setup`, `run`, `status`, `pause`, `resume`, `connect`, `find-node`) |
| `cmd/devrelay` | The real relay on localhost, for development |
| `cmd/fakenode` | A standalone fake companion node |
| `internal/bridge` | Node manager, relay session, reconnect backoff |
| `internal/companion` | Companion framing and the identity check (verified against MeshCore companion-v1.18.0) |
| `internal/nodelink` | Node link interface, TCP link, and LAN rediscovery of WiFi nodes |
| `internal/relayclient` | Client side of the relay protocol |
| `internal/connect` | `meshbridge connect`: local TCP companion port ↔ relay lease |
| `internal/control` | The bridge's local control API (status, pause, resume) and its client |
| `internal/svc` | Service install (systemd) |
| `internal/state` | The bridge's `state.json` (pause until resumed, last node address) |
| `internal/pairing` | The bridge side of pairing |
| `internal/relaytest`, `internal/fakenode` | Test doubles: a scripted relay and a fake node |
| `e2e` | End-to-end tests: local (real relay in process) and live (production relay) |

## License

MIT
