# sensive-go/grpc

Sensive access control for Go gRPC services, imported as `sensivegrpc`.

Sensive uses your organization's access policies to decide whether a
person or AI agent inside your organization may perform an operation. You
describe what each method does with data as protobuf options in your
service's own `.proto` files. The server interceptors in this module read
those options on every call, recognize who is calling, ask the Dynamic
Access Checker, and let the call through or refuse it.

```bash
go get github.com/sensive-ai/sensive-go/grpc
```

```go
import (
    sensivegrpc "github.com/sensive-ai/sensive-go/grpc"
    // The client: OwnerID and OwnerEmail, and a Checker of your own.
    sensiveclient "github.com/sensive-ai/sensive-go/client"
    // The request types: the Owner an owner lookup returns.
    accessv1 "github.com/sensive-ai/sensive-go/client/gen/dynax/access/v1"
)
```

## How it works

- **Only annotated methods are gated.** A method with a Sensive `operation`
  option is checked; any other method, health checks and reflection
  included, is never touched.
- **Only your internal actors are checked.** A function you write, the
  principal resolver, looks at each call and returns the employee or AI
  agent behind it, or `nil`. A call with no actor, such as one from your
  product's own users or from another service, passes untouched and Sensive
  is never contacted.
- **The owner is the customer whose data a call touches.** A method names
  the request field holding the customer's key, and one lookup you write
  turns that key into the owner, so policies can take into account whose
  data it is.
- **Two tokens travel as metadata.** A caller who has justified their
  access sends a justification token in `dynax-justification-token`. A
  response the checker decided carries its signed decision token in
  `dynax-decision-token`, allowed or denied.
- **The mode decides what a decision does.** In `ENFORCED` mode the
  checker's decision stands. In `LOG_ONLY` mode every call proceeds and the
  checker records what it would have decided. In `DISABLED` mode nothing
  is checked.

## 1. Describe your methods

Import the Sensive options, declare the application once per service, and
put an `operation` on each method that touches data:

```protobuf
syntax = "proto3";

package acme.ledger.v1;

import "sensive/v1/annotations.proto";

option go_package = "example.com/ledger/acme/ledger/v1;ledgerpb";

service Transactions {
  option (sensive.v1.application) = {
    name: "ledger"
    categories: ["financial"]
  };

  rpc ListTransactions(ListTransactionsRequest) returns (ListTransactionsResponse) {
    option (sensive.v1.operation) = {
      capability: {
        sources: [APPLICATION_USER]
        semantic_roles: [CORE_CONTENT]
        action_types: [LIST]
      }
      usage: {
        subjects: [USER]
        source_details: [USER]
      }
      owner_field: "account_id"
    };
  }
}

message ListTransactionsRequest {
  string account_id = 1;
}
```

- `application` names the application in your policies and the access
  log, with its categories. `default_application` on the file applies to
  every service in it that declares none.
- `capability` says what the method does with data, whoever calls it:
  where the data comes from, what role it plays, and what the method does
  with it.
- `usage` says how your organization uses the method: whose data it is
  (`subjects`, required on every method) and, when the data comes from
  your application's users, which of their data (`source_details`, present
  exactly when `sources` contains `APPLICATION_USER`). A `subjects` that
  contains `NONE` contains nothing else.
- `owner_field` names the request's string field that holds the owner's
  key. A method whose request has no single such field sets
  `has_owner: true` instead and gets a resolver of its own (step 3).
- `name` is the operation's name in policies and the access log. It
  defaults to the method's full name,
  `acme.ledger.v1.Transactions.ListTransactions`.

Every value is defined by its comment in
`proto/sensive/taxonomy/v1/taxonomy.proto`, and every option and rule in
`proto/sensive/v1/annotations.proto`.

**Compiling.** Put this module's `proto/` directory on your proto import
path, and generate Go code for your own protos only. The Sensive protos'
Go bindings ship in these modules and the protos name them in their
`go_package`, so your generated code imports them as they are; generating
them a second time registers the same files twice, which panics at
startup. With `protoc`:

```sh
protoc -I . -I "$(go list -m -f '{{.Dir}}' github.com/sensive-ai/sensive-go/grpc)/proto" \
  --go_out=. --go_opt=paths=source_relative \
  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
  acme/ledger/v1/ledger.proto
```

With `buf`, the directory has to be inside your workspace: copy it in,
list it as a module in `buf.yaml`, and generate with `--path` naming your
own protos. If your `buf.gen.yaml` sets a managed-mode `go_package_prefix`,
scope it to your own protos with `path:` so it leaves the Sensive files
alone.

## 2. Recognize who is calling

```go
func principal(_ context.Context, md metadata.MD, _ string) *sensivegrpc.Actor {
    if v := md.Get("x-workforce-user"); len(v) > 0 && v[0] != "" { // set only by a trusted hop
        return sensivegrpc.Human(v[0])
    }
    return nil
}
```

The resolver runs on every call, so it stays synchronous and does no I/O.
Return an actor only when a verified source positively identifies one,
never because a field is missing. A call the resolver does not recognize
is not checked, so internal traffic has to carry a signal your
infrastructure sets and outside callers cannot. A resolver that panics is
treated as returning `nil`.

| Actor | Meaning |
|---|---|
| `sensivegrpc.Human(email)` | An employee, by their workforce email |
| `sensivegrpc.Agent(issuer, id)` | An internal AI agent |
| `.WithEmail(email)` | The agent's own email identity |
| `.WithOnBehalfOf(email)` | The employee an agent acts for, under a verified delegation; access is evaluated as that employee |
| `.WithSource(s)` | Which signal identified the actor, for logs only |

Policies are keyed on email, so an agent with neither an email nor an
employee it acts for cannot be checked, and in `ENFORCED` mode its calls
are denied.

## 3. Find the owner

```go
func owner(ctx context.Context, key string) (*accessv1.Owner, error) {
    account, err := accounts.Get(ctx, key)
    if err != nil {
        return nil, err
    }
    return sensiveclient.OwnerID(account.CustomerID), nil
}
```

One lookup serves every method with an `owner_field`: the interceptor reads
that field from the request and passes its value. An empty field is no
owner, and the lookup is not called. Return `sensiveclient.OwnerID(id)` or
`sensiveclient.OwnerEmail(email)`, setting both fields of the `Owner` when you know
both; `nil, nil` for no owner; or an error, which fails the check.

A method with `has_owner` gets a resolver of its own, typed by its request:

```go
sensivegrpc.Owner(ledgerpb.Transactions_Transfer_FullMethodName,
    func(ctx context.Context, req *ledgerpb.TransferRequest) (*accessv1.Owner, error) {
        return sensiveclient.OwnerID(req.GetSource().GetCustomerId()), nil
    })
```

## 4. Install the interceptors

```go
gate, err := sensivegrpc.FromEnv(principal, sensivegrpc.OwnerLookup(owner))
if err != nil {
    log.Fatal(err)
}
defer gate.Close()

server := grpc.NewServer(
    grpc.ChainUnaryInterceptor(authenticate, gate.Unary()),
    grpc.ChainStreamInterceptor(authenticateStream, gate.Stream()),
)
ledgerpb.RegisterTransactionsServer(server, &transactions{})

if err := gate.Verify(server); err != nil {
    log.Fatal(err)
}
```

- **Chain the interceptors after your authentication**, so the resolver
  sees an authenticated call.
- **`Verify`** runs once, after every service is registered and before
  `Serve`. It checks every annotation, that every method with an owner can
  resolve it and every resolver you registered is used, and that no
  streaming method declares an owner. It returns every problem, joined,
  and logs one line of coverage naming the methods that are not gated:
  `dynax_coverage gated=N ungated=M ungated_methods=[...]`.
- **`Close`**, once the server has stopped, closes the client the
  interceptors built, first sending the `LOG_ONLY` checks still in flight.
- **`New(cfg, opts...)`** is `FromEnv` with a `Config`: the resolver, a
  `Logger` (default `slog.Default()`), and a `Checker` to use in place of
  the client `FromEnv` builds, such as one `sensiveclient.FromEnv` built with dial
  options, or a stand-in for tests. A `Checker` you pass is yours to close.

## 5. Configure the deployment

The client reads its settings from the environment, and only from it:

| Variable | What it is | Default |
|---|---|---|
| `DYNAX_DAC_URL` | The Dynamic Access Checker's URL | Required |
| `DYNAX_SDK_KEY` | The deployment's API key, sent on every check when set | None: a check carries no key |
| `DYNAX_ENVIRONMENT` | `PRODUCTION_DATA` or `TEST_DATA`: whether this deployment serves real data | `PRODUCTION_DATA` |
| `DYNAX_TIMEOUT_MS` | How long one check may take, in milliseconds | `5000` |
| `DYNAX_MODE` | `ENFORCED`, `LOG_ONLY` or `DISABLED` | `ENFORCED` |

Values are trimmed, an empty value counts as unset, and a value from a
list is spelled exactly as shown. In `DISABLED` mode no other variable is
read. `FromEnv` returns a `*sensiveclient.ConfigError` naming any variable it
cannot read, and logs once what it read, never the key.

To introduce Sensive to a running service, start in `LOG_ONLY` mode: every
call proceeds without waiting on the checker, and the access log shows
what your policies would have decided. Switch to `ENFORCED` once they
decide as you intend.

## What callers see

| Case | The call | Decision token |
|---|---|---|
| Allowed | The handler runs | In the response header metadata, in `ENFORCED` mode |
| Denied by policy | Fails with `PermissionDenied`, reason `ACCESS_DENIED` | In the response header metadata and the error |
| The checker cannot be reached, times out or fails | Fails with `PermissionDenied`, reason `CHECKER_UNAVAILABLE` | None |
| An agent with no identity policies can evaluate | Fails with `PermissionDenied`, reason `ACTOR_NOT_EVALUABLE` | None |
| An invalid annotation, or an owner lookup or resolver that fails or panics | Fails with `PermissionDenied`, reason `MISCONFIGURED` | None |

The last three deny in `ENFORCED` mode only; in `LOG_ONLY` and `DISABLED`
mode the call proceeds. A call with no actor always proceeds, even to a
method whose annotation is invalid. A panic in your owner code never
crashes the server.

A denial's status carries an `errdetails.ErrorInfo` in the domain
`dynax.co` with the reason and, for `ACCESS_DENIED`, the decision token,
so the token survives a chain of callers. `sensivegrpc.Denial(err)` reads
them back:

```go
if reason, token, ok := sensivegrpc.Denial(err); ok {
    // reason is one of the sensivegrpc.Reason* constants.
}
```

Streams are checked once, when they open, before any message, so a
streaming method has no owner.

## Behind a gateway

Anything in front of the service must pass the two tokens' headers
unchanged. Envoy, Connect and gRPC-Web do. grpc-gateway renames headers by
default, so wrap its two header matchers where the mux is built:

```go
mux := runtime.NewServeMux(
    runtime.WithIncomingHeaderMatcher(sensivegrpc.IncomingHeaderMatcher(runtime.DefaultHeaderMatcher)),
    runtime.WithOutgoingHeaderMatcher(sensivegrpc.OutgoingHeaderMatcher(runtime.DefaultHeaderMatcher)),
)
```

Each passes its token through and leaves every other header to the
matcher it wraps; if the mux already sets one, wrap that one instead. A
denial needs nothing more: grpc-gateway's default error handler answers
`PermissionDenied` with 403 and forwards the decision token. This module
does not depend on grpc-gateway; the wrappers are plain functions of the
shape it takes.

## Calling other services

Sensive checks at the service a person calls, and the calls that service
makes in turn run under its own authority, so they need nothing. Only a
connection that forwards the caller's identity to a service that checks
it too takes the client interceptors, so the justification travels with
the identity:

```go
conn, err := grpc.NewClient(target,
    grpc.WithUnaryInterceptor(sensivegrpc.UnaryClient()),
    grpc.WithStreamInterceptor(sensivegrpc.StreamClient()))
```

## Testing

Run tests with `DYNAX_MODE=DISABLED` and every call reaches its handler
with no checker. To test the gating itself, pass a stand-in as
`Config.Checker`: anything with the `sensiveclient.AccessChecker` methods,
`CheckAccess` returning the `sensiveclient.Result` you want and `Failed` returning
the result for a check that could not be built.

## What the module contains

`proto/` holds the Sensive options (`sensive/v1/annotations.proto`) and the
taxonomy they use, which your protos compile against. `gen/` holds the
options' Go bindings, generated from `proto/`. The module's own tests, run
with `go test ./...`, read the cases in `testdata/`.

## License

Apache-2.0: see [LICENSE](../LICENSE).
