# Orbit WebRTC Go security guidance

WebRTC provides encrypted media/data transport after ICE/DTLS negotiation, but it does not
authenticate application users or authorize access to a tenant, room, or resource.

## Signaling access

Configure an `Authorize` callback to validate the request identity before WebSocket upgrade. The
callback must return `nil` only after explicit authorization; do not rely on Origin as an identity
signal. With no Origin header, native clients are allowed. When present, Origin must appear exactly
once and match an explicit configured HTTP(S) origin; wildcard and duplicate configuration entries
are rejected. Origin policy is browser cross-site defense, not authentication or network access
control.

Rate-limit public requests at ingress and set application-specific per-user/tenant peer quotas.
Peer slots bound authorization plus established leases, but the package does not identify users,
persist sessions, coordinate quotas across replicas, or provide DDoS protection.

## Offers, secrets, and diagnostics

Signal frames, SDP, media sections, peer counts, and data channels are bounded. Duplicate top-level
JSON keys are rejected to avoid ambiguous protocol interpretation. These checks reduce resource
exhaustion but do not make attacker-supplied SDP intrinsically safe; keep Pion current and validate
against the supported Go version and deployment.

Never log full SDP, candidate addresses, media/data, credentials, authorization headers, or raw
provider errors. `ICEConfig.String()` and Go formatting verbs redact username and TURN credential
values, but direct fields remain accessible to application code. Keep credentials out of source,
command arguments, and telemetry. Use short-lived TURN credentials and TLS-protected signaling.

## Lifecycle and reporting

Call `Shutdown` with a bounded context and verify peer cleanup. A shutdown deadline expiring means
cleanup may still be incomplete. Test the selected ingress, proxy trust, UDP/TURN path, and browser
authorization behavior. Follow the repository [security policy](../../SECURITY.md) for
vulnerability reports; do not publish real signaling payloads or service credentials in issues.
