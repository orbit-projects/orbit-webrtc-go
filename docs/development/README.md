# Development

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

Local peer tests use loopback ICE. Browser, TURN, NAT traversal, load, and multi-instance checks need
separate infrastructure and should be reported independently.

On 2026-10-10, all eleven test groups, race detector, `go vet`, and `go build` passed with Go 1.24.0.
Cross-package statement coverage was 76.0%, above the 75% CI floor. Hosted CI and public-network
interoperability remain separate release checks.
