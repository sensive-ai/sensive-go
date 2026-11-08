# sensive-go/client

The Go client of the Sensive Dynamic Access Checker, imported as `sensiveclient`.

Sensive uses your organization's access policies to decide whether a
person or AI agent inside your organization may perform an operation. This
client asks the checker for one decision at a time, over gRPC, and always
answers `ALLOW` or `DENY`.

## When to use it

Use the client directly when your service already has its own permission
layer, a central authorization service for example, and you want a Sensive
decision inside it. A gRPC service that wants its methods gated
declaratively uses `github.com/sensive-ai/sensive-go/grpc` instead, which is
built on this client and builds the requests for you.

## Configuration

The client reads its settings from the environment, and only from it:

| Variable | What it is | Default |
|---|---|---|
| `SENSIVE_DAC_URL` | The Dynamic Access Checker's URL | Required |
| `SENSIVE_ENVIRONMENT` | `PRODUCTION_DATA` or `TEST_DATA`: whether this deployment serves real data | `PRODUCTION_DATA` |
| `SENSIVE_TIMEOUT_MS` | How long one check may take, in milliseconds | `5000` |
| `SENSIVE_MODE` | `ENFORCED`, `LOG_ONLY` or `DISABLED` | `ENFORCED` |

Values are trimmed, an empty value counts as unset, and a value from a
list is spelled exactly as shown. In `DISABLED` mode no other variable is
read.

```go
client, err := sensiveclient.FromEnv()
if err != nil {
    log.Fatal(err) // a *sensiveclient.ConfigError naming the variable
}
defer client.Close()
```

`FromEnv` logs once what it read. It takes gRPC dial options for the
connection only, such as a private certificate authority, interceptors or
keepalive; they never change where the client connects. Build one client at startup and share it: it is safe for
concurrent use. `Close` waits for the `LOG_ONLY` checks still in flight and
closes the connection.

## Checking access

```go
import (
    sensiveclient "github.com/sensive-ai/sensive-go/client"
    accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
    taxonomyv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/taxonomy/v1"
    "google.golang.org/protobuf/proto"
)

result := client.CheckAccess(ctx, &accessv1.AccessCheckRequest{
    Principal:   &accessv1.Principal{Email: proto.String("jane@acme.com")},
    Application: proto.String("ledger"),
    Operation:   proto.String("customer.transactions.list"),
    Capability: &taxonomyv1.CapabilityDescriptors{
        Sources:       []taxonomyv1.Source_Value{taxonomyv1.Source_APPLICATION_USER},
        SemanticRoles: []taxonomyv1.SemanticRole_Value{taxonomyv1.SemanticRole_CORE_CONTENT},
        ActionTypes:   []taxonomyv1.ActionType_Value{taxonomyv1.ActionType_LIST},
    },
    Usage: &taxonomyv1.UsageDescriptors{
        Subjects:      []taxonomyv1.Subject_Value{taxonomyv1.Subject_USER},
        SourceDetails: []taxonomyv1.SourceDetail_Value{taxonomyv1.SourceDetail_USER},
    },
    Owner:        sensiveclient.OwnerID("cust_123"),
    ContextToken: proto.String(contextToken),
})

if !result.Allowed() {
    // Refuse, and return result.DecisionToken to the caller when it is
    // set, so they can justify their access and retry.
}
```

| Field | What it is |
|---|---|
| `Principal` | The employee or AI agent asking, by email |
| `Application` | The application's name in your policies and the access log |
| `Operation` | The operation's name in your policies and the access log |
| `Capability` | What the operation does with data, whoever calls it |
| `Categories` | The application's categories, from the taxonomy's category values; optional |
| `Usage` | How your organization uses the operation: whose data it is (`Subjects`, required) and, when the data comes from your application's users, which of their data (`SourceDetails`) |
| `Owner` | The customer whose data the request touches, when it names one: `sensiveclient.OwnerID(id)` or `sensiveclient.OwnerEmail(email)`, with both fields set when you know both |
| `ResourceId` | The specific resource the request touches, when it names one |
| `ContextToken` | The context token the caller sent, in the `sensive-context-token` header; unset when there is none |

Every taxonomy value is defined by its comment in
`proto/sensive/taxonomy/v1/taxonomy.proto`. In Go a value is named for its
type and itself: `APPLICATION_USER` is `taxonomyv1.Source_APPLICATION_USER`.
The client sets the environment from `SENSIVE_ENVIRONMENT`, and the mode,
on every check, whatever the request carries.

## Modes and results

`CheckAccess` always returns `ALLOW` or `DENY`, never an error:

| Case | The check | Result | Decision token |
|---|---|---|---|
| `ENFORCED`, the checker decides | Sent and waited on | The checker's `ALLOW` or `DENY` | Yes |
| `ENFORCED`, timed out or failed | Sent and waited on | `DENY` | No |
| `LOG_ONLY` | Sent in the background | `ALLOW`, at once | No |
| `DISABLED` | Not sent | `ALLOW`, at once | No |

In `LOG_ONLY` mode the caller never waits on Sensive: the connection and the
call happen after `CheckAccess` has returned, and the checker records the
decision it would have made, marked as not enforced. To introduce Sensive to
a running service, start in `LOG_ONLY` and switch to `ENFORCED` once the
access log shows your policies deciding as you intend.

Act on `result.Decision` alone. `result.Source` says where a result came
from (`SourceChecker`, `SourceTimeout`, `SourceFailure` or `SourceMode`),
and `result.Err` holds the failure behind a timeout or failure; both are
for logging.

When you cannot build a request at all, because the lookup of the owner
failed for example, `client.Failed(cause)` is the result to act on: `DENY`
in `ENFORCED` mode and `ALLOW` in the other two, the same as a check that
failed.

## Testing

Run tests with `SENSIVE_MODE=DISABLED` and every check is `ALLOW`, with no
checker. Code that takes a `sensiveclient.AccessChecker`, the two methods
`CheckAccess` and `Failed`, can be given a stand-in that returns the
`sensiveclient.Result` a test needs.

## What the module contains

`proto/` is the Dynamic Access Checker's API as a client sees it, with the
taxonomy it uses. `gen/` holds the Go
bindings, generated from `proto/`; `buf generate proto` with the module's
`buf.gen.yaml` reproduces it. The module depends on grpc-go and protobuf
and nothing else. Its own tests, run with `go test ./...`, read the cases in
`testdata/`.

## License

Apache-2.0: see [LICENSE](../LICENSE).
