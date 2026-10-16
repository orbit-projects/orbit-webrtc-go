# Orbit WebRTC Go operations

Run the service behind a trusted TLS-terminating ingress that forwards the configured signaling
path and exactly one Origin header when one is present. The Go listener does not terminate TLS by
itself. Restrict its bind address and network access so clients cannot bypass ingress identity or
rate limits.

## Capacity and configuration

`NewConfig()` defaults to 64 peers, 16 media sections, 32 data channels, and 192 KiB SDP. Hard
maxima are 4,096 peers, 64 media sections, 256 data channels, 256 KiB SDP, 64 allowed origins, 16
ICE server entries, and 16 URLs per ICE entry. One peer slot is reserved before authorization and
held for the full signaling lease, limiting callback/handshake work as well as connected peers.
These bounds are per server process; account for replica count and measured media/network workload.

The offer deadline covers receiving and negotiating the offer through ICE gathering. The control
lease may remain open after that deadline. `Shutdown(ctx)` closes active peer and signaling state,
then waits for handler cleanup until the caller's context expires. Provide a context with an
explicit deadline. When `ListenAndServe()` returns `http.ErrServerClosed` after a planned shutdown,
handle that as normal lifecycle completion.

## ICE and media connectivity

Configure STUN/TURN explicitly. No relay is embedded, and signaling success does not establish an
ICE path. TURN credentials should be short-lived and loaded from a secret manager. Verify UDP
reachability, relay fallback, TLS, codec compatibility, browser behavior, proxy idle timeouts, and
the selected network topology. Monitor peer counts and negotiation failures in host-owned
observability; this package exports no metrics endpoint or health service.

## Release validation

Run `go test -race ./...`, `go vet ./...`, and `go build ./...`. Then validate browser and Python
protocol interoperability, max-peer overload responses, negotiation timeouts, shutdown with
active peers, TURN fallback, NAT traversal, and representative sustained media/data load. Local
loopback tests do not verify public-network connectivity or release provenance.
