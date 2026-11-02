# Orbit WebRTC Go architecture

`orbit-webrtc-go` is a standalone Go WebRTC peer endpoint using Pion. It implements the same v1
WebSocket signaling envelope as Python `orbit-webrtc` and is selected as a complete implementation
variant. Core does not proxy frames or own Pion peer connections.

```text
browser/client ── WSS signaling (offer/answer/control lease) ──> orbit-webrtc-go
       └──────── ICE/DTLS/SRTP/SCTP media and data ────────────> peer endpoint
```

Each accepted signaling connection owns one server-side peer. The server receives one complete
gathered SDP offer, builds a Pion peer, returns a gathered answer, then holds the WebSocket as a
control lease. A close message or socket disconnect releases the peer. Trickle ICE, renegotiation,
rooms, SFU routing, recording, transcoding, and cross-instance coordination are not implemented.

## Contract and resource boundaries

The signaling frame is capped at 256 KiB; configured SDP is capped at 192 KiB by default and at
256 KiB maximum. Offers must contain exactly `v`, `type`, and `sdp`, with no duplicate top-level
JSON keys. The server counts SDP `m=` sections before allocating the Pion peer and checks the
resulting transceivers after Pion parses it. Peer slots cover both authorization/upgrade work and
active peer leases. Peer/media/data channel counts are configurable within hard limits.

Origin matching is exact against configured HTTP(S) origins. A missing Origin is allowed for native
clients; browser origin checks are not authentication. The optional `Authorize` callback runs
before WebSocket upgrade. Applications own identity, per-user quotas, room membership, and
application-level permissions. Pion callbacks own media/data handling; unhandled tracks are drained
and discarded to bound receiver queues.

## Lifecycle and dependency ownership

The application or service supervisor owns `Server` lifecycle. `ListenAndServe()` creates the HTTP
listener and `Shutdown(ctx)` closes accepted signaling connections, closes Pion peers, and waits
within the supplied context. The server owns peer connections created from accepted offers; it does
not own external TURN services, credential retrieval, or application callback resources.

The package uses Pion and `coder/websocket`, not a Python process-plugin bridge. Go is the complete
peer service for deployments that choose this variant; no claim is made that it is faster for every
network or media workload. Compare with Python using representative browsers, codecs, ICE topology,
and throughput before selecting a variant.

The implementation is an alpha review candidate after Go 1.24.0 tests, race detection, vet, and
build passed locally. Cross-language signaling conformance, browser and network deployment behavior,
hosted CI, and release provenance remain release gates.
