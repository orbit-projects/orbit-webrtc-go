# 0001: Match Python's bounded signaling protocol

- Status: accepted
- Date: 2026-10-06

## Context

Orbit needs a complete Go WebRTC peer implementation, not generated bindings that forward work to
Python. The package must be selectable alongside Python through one documented protocol.

## Decision

Implement the same v1 JSON offer/answer signaling contract as `orbit-webrtc`: one complete client
offer, one server answer after ICE gathering, and a WebSocket lease lasting until close/disconnect.
Use Pion for peer, media, and data-channel work. Keep media off the signaling connection.

## Consequences

There is no trickle ICE, renegotiation, SFU, or shared room state in v1. Cross-language browser and
network interoperability remains a release gate. Applications supply authorization and ICE servers.
