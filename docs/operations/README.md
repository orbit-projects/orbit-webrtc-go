# Operations

Bind signaling to a private address and terminate WSS at a trusted ingress. Ensure it forwards the
configured path and Origin header. Configure explicit ICE servers and TURN when networks require
relaying; signaling success does not prove that a media path can be established.

Set peer/media/data-channel limits from measured resources. Configure ingress and termination
deadlines to drain signaling leases. `Shutdown(ctx)` closes active signaling and Pion peers before
returning. Test supported browsers, codecs, NATs, UDP/TURN paths, and ingress topology in deployment.
