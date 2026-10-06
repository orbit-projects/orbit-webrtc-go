# Orbit WebRTC Go

`orbit-webrtc-go` is the Go WebRTC peer implementation for Orbit. It uses Pion WebRTC and exposes
the same v1 WebSocket signaling contract as Python `orbit-webrtc`: one complete SDP offer, one
gathered SDP answer, then a signaling lease kept open until close/disconnect. Media and data use
direct WebRTC ICE/DTLS/SRTP/SCTP transport and do not pass through Core or the signaling socket.

V1 is a peer endpoint, not an SFU. Applications provide authorization and callbacks for Pion peer
connections, remote tracks, and data channels. Unhandled tracks are drained and discarded. Configure
STUN/TURN explicitly for deployment networking; there are no bundled servers or default
credentials. TURN credentials should be short-lived and supplied from secret management.

## Install and use

```sh
go get github.com/orbit-projects/orbit-webrtc-go
```

```go
package main

import (
    "log"

    webrtc "github.com/orbit-projects/orbit-webrtc-go"
    pion "github.com/pion/webrtc/v4"
)

func main() {
    config := webrtc.NewConfig()
    config.AllowedOrigins = []string{"https://app.example"}
    config.OnDataChannel = func(channel *pion.DataChannel) {
        channel.OnMessage(func(message pion.DataChannelMessage) {
            _ = channel.Send(message.Data)
        })
    }
    server, err := webrtc.NewServer(config)
    if err != nil {
        log.Fatal("invalid WebRTC configuration")
    }
    if err := server.ListenAndServe(); err != nil {
        log.Fatal("WebRTC signaling listener stopped")
    }
}
```

Set exact browser origins, an authorization callback, and explicit ICE server configuration. With
no Origin header, native clients are allowed; browser origins must match `AllowedOrigins`. The
server bounds peers, SDP, media sections, and data channels, and rejects malformed signaling.

## Signaling protocol

Connect to `/rtc` over WSS and send `{"v":1,"type":"offer","sdp":"..."}` with a complete SDP
offer after local ICE gathering. Receive `{"v":1,"type":"answer","sdp":"..."}`. Trickle ICE and
renegotiation are not part of v1. Keep the signaling socket open as the peer's control lease; send
`{"v":1,"type":"close"}` or disconnect to release the peer.

See [architecture](docs/architecture/overview.md), [operations](docs/operations/README.md),
[security](docs/security/overview.md), [development](docs/development/README.md), and the
[ADR index](docs/architecture/adr/README.md).

This package is pre-alpha. Browser/codec interoperability, TURN/TLS, NAT reachability, ingress,
load/soak, and release provenance remain open gates. It does not implement an SFU, rooms, recording,
transcoding, or cross-instance coordination.

Licensed under Apache-2.0.
