# Security

With no Origin header, native clients are allowed; browser requests must match the exact configured
origin list. Add an `Authorize` callback for application identity before upgrade. Bound offers,
peers, media sections, and data channels. Never log SDP, candidate addresses, TURN credentials,
authorization headers, or user media.

WebRTC encryption does not grant application identity or authorization. Terminate WSS at a trusted
ingress, issue short-lived TURN credentials, rate-limit unauthenticated offers, and enforce tenant
and peer quotas in application policy. Do not embed secrets in source or process arguments.
