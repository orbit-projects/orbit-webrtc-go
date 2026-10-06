# Architecture

This package implements the `orbit-webrtc` v1 signaling JSON protocol using Go and Pion. Each
signaling WebSocket owns one server-side peer connection. It gathers server ICE before returning an
answer. The websocket is a control lease; negotiated media/data flows over ICE directly.

Application callbacks own identity policy and Pion-specific track/data-channel processing. Default
limits cap peers, SDP bytes, media sections, and data channels. Unhandled tracks are read and
discarded. Core's local process gRPC host is not involved.
