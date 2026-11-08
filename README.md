# sensive-go

The Go SDK for Sensive access control.

Sensive uses your organization's access policies to decide whether a
person or AI agent inside your organization may perform an operation. This
SDK asks the Sensive Dynamic Access Checker for those decisions.

## Getting started

The SDK is two Go modules. Most services want `grpc`; use `client` on its
own when you have an authorization layer of your own, or a framework
without an integration.

| Module | Package | Use it for |
|---|---|---|
| [`grpc`](./grpc/README.md) | `sensivegrpc` | Gating a Go gRPC service: server interceptors that read the access-control options on each method and ask the checker |
| [`client`](./client/README.md) | `sensiveclient` | Asking the checker directly |

```bash
go get github.com/sensive-ai/sensive-go/grpc     # the gRPC integration
go get github.com/sensive-ai/sensive-go/client   # the client on its own
```

Then follow that module's README, which walks you through setting it up.
The gRPC module is built on the client and brings it with it.

You'll need your deployment's checker URL. The SDK reads it from the
environment, as `SENSIVE_DAC_URL`, and each module's README lists every
setting.

## Versions

Each module is versioned on its own, with tags named for its directory:
`client/v0.1.0`, `grpc/v0.1.0`. The SDK follows semantic versioning and is
at v0, so a minor version may change the API. A version, once tagged, is
never moved or replaced. Each module's `go.mod` names the Go release and
the dependency versions it requires.

## Security

To report a vulnerability, see the organization's
[security policy](https://github.com/sensive-ai/.github/blob/main/SECURITY.md).

## License

Apache-2.0: see [LICENSE](./LICENSE).
