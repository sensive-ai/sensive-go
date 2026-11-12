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

The SDK follows semantic versioning and is at v0:

- A patch release (`v0.1.0` to `v0.1.1`) fixes or adds without breaking
  anything, so it is always safe to take.
- A minor release (`v0.1.0` to `v0.2.0`) may change the API or the
  configuration, or require a newer Go or newer versions of the SDK's
  dependencies. Its
  [release notes](https://github.com/sensive-ai/sensive-go/releases) say
  what to change.

Use the same minor version of both modules: `grpc` v0.2.x with `client`
v0.2.x.

Each module requires the Go release its `go.mod` names, or a newer one,
and no newer version of a dependency than the SDK needs, so adding the
SDK moves your program's dependencies as little as it can. Each is tested
at those versions and at the newest.

## Security

To report a vulnerability, see the organization's
[security policy](https://github.com/sensive-ai/.github/blob/main/SECURITY.md).

## License

Apache-2.0: see [LICENSE](./LICENSE).
