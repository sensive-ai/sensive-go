// Copyright 2026 Sensive, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sensivegrpc

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	sensiveclient "github.com/sensive-ai/sensive-go/client"
	accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
	conformancev1 "github.com/sensive-ai/sensive-go/grpc/internal/conformancepb/conformance/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeChecker stands in for the client: it records the requests it is
// asked and returns what the client would. In ENFORCED mode that is the
// decision it is told, with a token, or DENY for a failure; with
// notEnforced set, the client's mode is LOG_ONLY or DISABLED, and every
// result is ALLOW.
type fakeChecker struct {
	mu          sync.Mutex
	requests    []*accessv1.AccessCheckRequest
	tokens      []string
	failures    []error
	result      accessv1.AccessDecision_Value
	err         error
	notEnforced bool
}

var notEnforced = sensiveclient.Result{Decision: accessv1.AccessDecision_ALLOW, Source: sensiveclient.SourceMode}

func (f *fakeChecker) CheckAccess(_ context.Context, req *accessv1.AccessCheckRequest) sensiveclient.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	f.tokens = append(f.tokens, req.GetContextToken())
	switch {
	case f.notEnforced:
		return notEnforced
	case f.err != nil:
		return sensiveclient.Result{Decision: accessv1.AccessDecision_DENY, Source: sensiveclient.SourceFailure, Err: f.err}
	}
	return sensiveclient.Result{Decision: f.result, DecisionToken: "decision-" + f.result.String(), Source: sensiveclient.SourceChecker}
}

func (f *fakeChecker) Failed(cause error) sensiveclient.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, cause)
	if f.notEnforced {
		return notEnforced
	}
	return sensiveclient.Result{Decision: accessv1.AccessDecision_DENY, Source: sensiveclient.SourceFailure, Err: cause}
}

func (f *fakeChecker) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// ledger is the fixture service, recording whether a handler ran.
type ledger struct {
	conformancev1.UnimplementedLedgerServer
	mu      sync.Mutex
	handled []string
}

func (l *ledger) note(method string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.handled = append(l.handled, method)
}

func (l *ledger) ListTransactions(_ context.Context, _ *conformancev1.ListTransactionsRequest) (*conformancev1.ListTransactionsResponse, error) {
	l.note("ListTransactions")
	return &conformancev1.ListTransactionsResponse{TransactionIds: []string{"txn-1"}}, nil
}

func (l *ledger) GetBalance(_ context.Context, _ *conformancev1.GetBalanceRequest) (*conformancev1.GetBalanceResponse, error) {
	l.note("GetBalance")
	return &conformancev1.GetBalanceResponse{Cents: 100}, nil
}

func (l *ledger) GetStatement(_ context.Context, _ *conformancev1.GetStatementRequest) (*conformancev1.GetStatementResponse, error) {
	l.note("GetStatement")
	return &conformancev1.GetStatementResponse{Lines: []string{"opening balance"}}, nil
}

func (l *ledger) Health(_ context.Context, _ *conformancev1.HealthRequest) (*conformancev1.HealthResponse, error) {
	l.note("Health")
	return &conformancev1.HealthResponse{Ok: true}, nil
}

func (l *ledger) Tail(_ *conformancev1.TailRequest, stream grpc.ServerStreamingServer[conformancev1.TailResponse]) error {
	l.note("Tail")
	return stream.Send(&conformancev1.TailResponse{Line: "one"})
}

// workforce is a principal resolver that recognizes the `x-workforce-user`
// metadata stamped by a trusted hop and nothing else.
func workforce(_ context.Context, md metadata.MD, _ string) *Actor {
	if v := md.Get("x-workforce-user"); len(v) > 0 {
		return Human(v[0])
	}
	return nil
}

type harness struct {
	checker  *fakeChecker
	ledger   *ledger
	client   conformancev1.LedgerClient
	server   *grpc.Server
	icpt     *Interceptor
	listener *bufconn.Listener
}

// ownerFromAccount is the fixture's owner resolver: the account id is the
// customer id, as a lookup would find.
func ownerFromAccount(_ context.Context, req *conformancev1.ListTransactionsRequest) (*accessv1.Owner, error) {
	return sensiveclient.OwnerID("cust-of-" + req.GetAccountId()), nil
}

// lookupByAccount is the fixture's owner lookup, for the methods that name
// their owner field: the same mapping as ownerFromAccount.
func lookupByAccount(_ context.Context, key string) (*accessv1.Owner, error) {
	return sensiveclient.OwnerID("cust-of-" + key), nil
}

// fixtureOwners resolves every fixture method that declares an owner.
func fixtureOwners() []Option {
	return []Option{Owner(conformancev1.Ledger_ListTransactions_FullMethodName, ownerFromAccount), OwnerLookup(lookupByAccount)}
}

func newHarness(t *testing.T, cfg Config, opts ...Option) *harness {
	t.Helper()
	return newHarnessWith(t, cfg, nil, opts...)
}

// newHarnessWith also lets a test register more services on the server.
func newHarnessWith(t *testing.T, cfg Config, register func(*grpc.Server), opts ...Option) *harness {
	t.Helper()
	checker := &fakeChecker{result: accessv1.AccessDecision_ALLOW}
	if cfg.Checker == nil {
		cfg.Checker = checker
	}
	if cfg.PrincipalResolver == nil {
		cfg.PrincipalResolver = workforce
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if len(opts) == 0 {
		opts = fixtureOwners()
	}
	icpt, err := New(cfg, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(icpt.Unary()), grpc.StreamInterceptor(icpt.Stream()))
	svc := &ledger{}
	conformancev1.RegisterLedgerServer(server, svc)
	if register != nil {
		register(server)
	}
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &harness{checker: checker, ledger: svc, client: conformancev1.NewLedgerClient(conn), server: server, icpt: icpt, listener: lis}
}

func asWorkforce(ctx context.Context, email string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-workforce-user", email)
}

func TestAllowReachesTheHandlerWithTheDecisionToken(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := metadata.AppendToOutgoingContext(asWorkforce(context.Background(), "agent@example.com"), ContextTokenKey, "eyJ.jwe")

	var header metadata.MD
	resp, err := h.client.ListTransactions(ctx, &conformancev1.ListTransactionsRequest{AccountId: "acct-1"}, grpc.Header(&header))
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(resp.GetTransactionIds()) != 1 {
		t.Errorf("handler response = %+v", resp)
	}
	if got := header.Get(DecisionTokenKey); len(got) != 1 || got[0] != "decision-ALLOW" {
		t.Errorf("decision token header = %v", got)
	}
	if h.checker.calls() != 1 {
		t.Fatalf("checker calls = %d, want 1", h.checker.calls())
	}
	req := h.checker.requests[0]
	if req.GetPrincipal().GetEmail() != "agent@example.com" || req.GetApplication() != "ledger" ||
		req.GetOperation() != "conformance.v1.Ledger.ListTransactions" || req.GetUsage().Environment != nil ||
		req.GetOwner().GetId() != "cust-of-acct-1" {
		t.Errorf("request to the checker = %+v", req)
	}
	if h.checker.tokens[0] != "eyJ.jwe" {
		t.Errorf("context token passed = %q", h.checker.tokens[0])
	}
}

func TestDenyIsPermissionDeniedWithTheToken(t *testing.T) {
	h := newHarness(t, Config{})
	h.checker.result = accessv1.AccessDecision_DENY

	var header metadata.MD
	_, err := h.client.GetBalance(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.GetBalanceRequest{}, grpc.Header(&header))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v, want PermissionDenied", err)
	}
	reason, token, ok := Denial(err)
	if !ok || reason != ReasonDenied || token != "decision-DENY" {
		t.Errorf("Denial = (%q, %q, %v)", reason, token, ok)
	}
	if got := header.Get(DecisionTokenKey); len(got) != 1 || got[0] != "decision-DENY" {
		t.Errorf("decision token header = %v", got)
	}
	if len(h.ledger.handled) != 0 {
		t.Errorf("handler ran on a denial: %v", h.ledger.handled)
	}
}

func TestNoActorBypassesWithoutContactingTheChecker(t *testing.T) {
	h := newHarness(t, Config{})

	var header metadata.MD
	if _, err := h.client.GetBalance(context.Background(), &conformancev1.GetBalanceRequest{}, grpc.Header(&header)); err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if h.checker.calls() != 0 {
		t.Errorf("checker contacted for a call with no internal actor")
	}
	if len(header.Get(DecisionTokenKey)) != 0 {
		t.Errorf("a bypassed call carried a decision token")
	}
}

func TestUnannotatedMethodIsNotGated(t *testing.T) {
	h := newHarness(t, Config{})
	if _, err := h.client.Health(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.HealthRequest{}); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.checker.calls() != 0 {
		t.Errorf("checker contacted for an unannotated method")
	}
}

func TestCheckerFailureDenies(t *testing.T) {
	h := newHarness(t, Config{})
	h.checker.err = errors.New("connection refused")

	_, err := h.client.GetBalance(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.GetBalanceRequest{})
	reason, token, ok := Denial(err)
	if status.Code(err) != codes.PermissionDenied || !ok || reason != ReasonCheckerUnavailable || token != "" {
		t.Errorf("err = %v, Denial = (%q, %q, %v); want PermissionDenied CHECKER_UNAVAILABLE", err, reason, token, ok)
	}
}

func TestAgentWithoutEvaluableIdentityIsDenied(t *testing.T) {
	h := newHarness(t, Config{PrincipalResolver: func(context.Context, metadata.MD, string) *Actor {
		return Agent("https://login.example.com", "copilot")
	}})
	_, err := h.client.GetBalance(context.Background(), &conformancev1.GetBalanceRequest{})
	if reason, _, ok := Denial(err); !ok || reason != ReasonUnevaluableActor {
		t.Errorf("err = %v, want ACTOR_NOT_EVALUABLE", err)
	}
	if h.checker.calls() != 0 || len(h.checker.failures) != 1 {
		t.Errorf("an unevaluable actor was checked, or not decided by the client: %d checks, %d failures", h.checker.calls(), len(h.checker.failures))
	}
}

func TestAgentOnBehalfOfIsEvaluatedAsTheHuman(t *testing.T) {
	h := newHarness(t, Config{PrincipalResolver: func(context.Context, metadata.MD, string) *Actor {
		return Agent("https://login.example.com", "copilot").WithOnBehalfOf("engineer@example.com")
	}})
	if _, err := h.client.GetBalance(context.Background(), &conformancev1.GetBalanceRequest{}); err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if h.checker.requests[0].GetPrincipal().GetEmail() != "engineer@example.com" {
		t.Errorf("principal = %v, want the delegating human", h.checker.requests[0].GetPrincipal())
	}
}

func TestOwnerResolverFailureDenies(t *testing.T) {
	h := newHarness(t, Config{}, Owner(conformancev1.Ledger_ListTransactions_FullMethodName,
		func(context.Context, *conformancev1.ListTransactionsRequest) (*accessv1.Owner, error) {
			return nil, errors.New("account lookup failed")
		}))
	_, err := h.client.ListTransactions(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.ListTransactionsRequest{AccountId: "acct-1"})
	if reason, _, ok := Denial(err); !ok || reason != ReasonMisconfigured {
		t.Errorf("err = %v, want MISCONFIGURED", err)
	}
	if h.checker.calls() != 0 {
		t.Errorf("checker contacted without an owner")
	}
}

func TestOwnerFieldIsReadAndLookedUp(t *testing.T) {
	h := newHarness(t, Config{})
	ctx := asWorkforce(context.Background(), "agent@example.com")
	if _, err := h.client.GetStatement(ctx, &conformancev1.GetStatementRequest{AccountId: "acct-7"}); err != nil {
		t.Fatalf("GetStatement: %v", err)
	}
	if got := h.checker.requests[0].GetOwner(); got.GetId() != "cust-of-acct-7" {
		t.Errorf("owner = %+v, want cust-of-acct-7", got)
	}
}

func TestEmptyOwnerFieldIsNoOwner(t *testing.T) {
	called := false
	h := newHarness(t, Config{},
		Owner(conformancev1.Ledger_ListTransactions_FullMethodName, ownerFromAccount),
		OwnerLookup(func(context.Context, string) (*accessv1.Owner, error) {
			called = true
			return nil, errors.New("not reached")
		}))
	if _, err := h.client.GetStatement(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.GetStatementRequest{}); err != nil {
		t.Fatalf("GetStatement: %v", err)
	}
	if called {
		t.Error("the lookup was called with an empty key")
	}
	if got := h.checker.requests[0].GetOwner(); got != nil {
		t.Errorf("owner = %+v, want none", got)
	}
}

func TestOwnerLookupFailureDenies(t *testing.T) {
	h := newHarness(t, Config{},
		Owner(conformancev1.Ledger_ListTransactions_FullMethodName, ownerFromAccount),
		OwnerLookup(func(context.Context, string) (*accessv1.Owner, error) {
			return nil, errors.New("customer store unavailable")
		}))
	_, err := h.client.GetStatement(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.GetStatementRequest{AccountId: "acct-7"})
	if reason, _, ok := Denial(err); !ok || reason != ReasonMisconfigured {
		t.Errorf("err = %v, want MISCONFIGURED", err)
	}
	if h.checker.calls() != 0 {
		t.Errorf("checker contacted without an owner")
	}
}

func TestMissingOwnerResolverDeniesPerCall(t *testing.T) {
	// No options: the harness registers none, and ListTransactions declares
	// has_owner.
	h := newHarness(t, Config{}, func(*Interceptor) error { return nil })
	_, err := h.client.ListTransactions(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.ListTransactionsRequest{})
	if reason, _, ok := Denial(err); !ok || reason != ReasonMisconfigured {
		t.Errorf("err = %v, want MISCONFIGURED", err)
	}
}

func TestResolverPanicBypasses(t *testing.T) {
	h := newHarness(t, Config{PrincipalResolver: func(context.Context, metadata.MD, string) *Actor {
		panic("claims not loaded")
	}})
	if _, err := h.client.GetBalance(context.Background(), &conformancev1.GetBalanceRequest{}); err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if h.checker.calls() != 0 {
		t.Errorf("checker contacted after a resolver panic")
	}
}

func TestNotEnforcedLetsEveryCallThroughWithoutAToken(t *testing.T) {
	h := newHarness(t, Config{})
	h.checker.notEnforced = true
	h.checker.result = accessv1.AccessDecision_DENY

	var header metadata.MD
	if _, err := h.client.GetBalance(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.GetBalanceRequest{}, grpc.Header(&header)); err != nil {
		t.Fatalf("GetBalance when checks are not enforced: %v", err)
	}
	if h.checker.calls() != 1 || len(header.Get(DecisionTokenKey)) != 0 {
		t.Errorf("checks = %d, decision token header = %v; want the check asked and no token", h.checker.calls(), header.Get(DecisionTokenKey))
	}
}

func TestNotEnforcedLetsAFailedCheckThrough(t *testing.T) {
	h := newHarness(t, Config{PrincipalResolver: func(context.Context, metadata.MD, string) *Actor {
		return Agent("https://login.example.com", "copilot")
	}})
	h.checker.notEnforced = true
	if _, err := h.client.GetBalance(context.Background(), &conformancev1.GetBalanceRequest{}); err != nil {
		t.Fatalf("an unevaluable actor when checks are not enforced: %v", err)
	}
	if len(h.checker.failures) != 1 {
		t.Errorf("the client was not asked about the failed check")
	}
}

func TestStreamIsGatedAtOpen(t *testing.T) {
	h := newHarness(t, Config{})
	h.checker.result = accessv1.AccessDecision_DENY

	stream, err := h.client.Tail(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.TailRequest{})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	_, err = stream.Recv()
	if reason, _, ok := Denial(err); !ok || reason != ReasonDenied {
		t.Errorf("stream open err = %v, want ACCESS_DENIED", err)
	}
	if req := h.checker.requests[0]; req.GetOperation() != "conformance.v1.Ledger.Tail" || req.GetOwner() != nil {
		t.Errorf("stream request = %+v, want Tail with no owner", req)
	}

	h.checker.result = accessv1.AccessDecision_ALLOW
	stream, err = h.client.Tail(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.TailRequest{})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if msg, err := stream.Recv(); err != nil || msg.GetLine() != "one" {
		t.Errorf("Recv = %v, %v", msg, err)
	}
}

func TestVerify(t *testing.T) {
	t.Run("a complete configuration passes", func(t *testing.T) {
		h := newHarness(t, Config{})
		if err := h.icpt.Verify(h.server); err != nil {
			t.Errorf("Verify: %v", err)
		}
	})
	t.Run("has_owner without a resolver", func(t *testing.T) {
		h := newHarness(t, Config{}, func(*Interceptor) error { return nil })
		err := h.icpt.Verify(h.server)
		if err == nil || !strings.Contains(err.Error(), "declares has_owner but names no owner_field and has no owner resolver") {
			t.Errorf("Verify = %v", err)
		}
	})
	t.Run("a resolver on a method without has_owner", func(t *testing.T) {
		h := newHarness(t, Config{},
			Owner(conformancev1.Ledger_ListTransactions_FullMethodName, ownerFromAccount),
			Owner(conformancev1.Ledger_GetBalance_FullMethodName, func(context.Context, *conformancev1.GetBalanceRequest) (*accessv1.Owner, error) { return nil, nil }))
		err := h.icpt.Verify(h.server)
		if err == nil || !strings.Contains(err.Error(), "does not declare has_owner") {
			t.Errorf("Verify = %v", err)
		}
	})
	t.Run("a resolver typed by the wrong request", func(t *testing.T) {
		h := newHarness(t, Config{},
			Owner(conformancev1.Ledger_ListTransactions_FullMethodName, func(context.Context, *conformancev1.GetBalanceRequest) (*accessv1.Owner, error) { return nil, nil }))
		err := h.icpt.Verify(h.server)
		if err == nil || !strings.Contains(err.Error(), "the owner resolver takes conformance.v1.GetBalanceRequest") {
			t.Errorf("Verify = %v", err)
		}
	})
	t.Run("owner_field without an OwnerLookup", func(t *testing.T) {
		h := newHarness(t, Config{}, Owner(conformancev1.Ledger_ListTransactions_FullMethodName, ownerFromAccount))
		err := h.icpt.Verify(h.server)
		if err == nil || !strings.Contains(err.Error(), `names owner_field "account_id" but no OwnerLookup is registered`) {
			t.Errorf("Verify = %v", err)
		}
	})
	t.Run("owner_field and a resolver on one method", func(t *testing.T) {
		h := newHarness(t, Config{}, append(fixtureOwners(),
			Owner(conformancev1.Ledger_GetStatement_FullMethodName, func(context.Context, *conformancev1.GetStatementRequest) (*accessv1.Owner, error) { return nil, nil }))...)
		err := h.icpt.Verify(h.server)
		if err == nil || !strings.Contains(err.Error(), "names owner_field and also has an owner resolver") {
			t.Errorf("Verify = %v", err)
		}
	})
	t.Run("an OwnerLookup no method uses", func(t *testing.T) {
		icpt, err := New(Config{PrincipalResolver: workforce, Checker: &fakeChecker{}, Logger: slog.New(slog.DiscardHandler)}, OwnerLookup(lookupByAccount))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		server := grpc.NewServer()
		conformancev1.RegisterDefaultedServer(server, conformancev1.UnimplementedDefaultedServer{})
		err = icpt.Verify(server)
		if err == nil || !strings.Contains(err.Error(), "no method names an owner_field") {
			t.Errorf("Verify = %v", err)
		}
	})
	t.Run("a resolver for a method nobody serves", func(t *testing.T) {
		h := newHarness(t, Config{},
			Owner(conformancev1.Ledger_ListTransactions_FullMethodName, ownerFromAccount),
			Owner("/conformance.v1.Nobody/Serves", func(context.Context, *conformancev1.GetBalanceRequest) (*accessv1.Owner, error) { return nil, nil }))
		err := h.icpt.Verify(h.server)
		if err == nil || !strings.Contains(err.Error(), "which no registered service serves") {
			t.Errorf("Verify = %v", err)
		}
	})
}

func TestNewRequiresAResolver(t *testing.T) {
	if _, err := New(Config{Checker: &fakeChecker{}}); err == nil {
		t.Error("New accepted a configuration with no principal resolver")
	}
	if _, err := New(Config{PrincipalResolver: workforce, Checker: &fakeChecker{}},
		Owner(conformancev1.Ledger_ListTransactions_FullMethodName, ownerFromAccount),
		Owner(conformancev1.Ledger_ListTransactions_FullMethodName, ownerFromAccount)); err == nil {
		t.Error("New accepted two resolvers for one method")
	}
	if _, err := New(Config{PrincipalResolver: workforce, Checker: &fakeChecker{}},
		OwnerLookup(lookupByAccount), OwnerLookup(lookupByAccount)); err == nil {
		t.Error("New accepted two owner lookups")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("SENSIVE_DAC_URL", "https://dac.example.com/api/dac")
	t.Setenv("SENSIVE_MODE", "")
	gate, err := FromEnv(workforce, OwnerLookup(lookupByAccount))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if gate.lookup == nil || gate.checker == nil {
		t.Errorf("FromEnv did not build the client and apply the options: %+v", gate)
	}

	// The client reads the environment, and its refusal names the variable.
	t.Setenv("SENSIVE_MODE", "OFF")
	var refused *sensiveclient.ConfigError
	if _, err := FromEnv(workforce); !errors.As(err, &refused) || refused.Variable != "SENSIVE_MODE" {
		t.Errorf("err = %v, want the client's refusal of SENSIVE_MODE", err)
	}

	// DISABLED reads nothing else.
	t.Setenv("SENSIVE_MODE", "DISABLED")
	t.Setenv("SENSIVE_DAC_URL", "")
	if _, err := FromEnv(workforce); err != nil {
		t.Errorf("DISABLED without the checker's settings: %v", err)
	}
}

func TestClientInterceptorPropagatesTheContextToken(t *testing.T) {
	// A call arrives at a first hop carrying the token; the hop calls a
	// second service through the client interceptor, which must carry it on.
	incoming := metadata.NewIncomingContext(context.Background(), metadata.Pairs(ContextTokenKey, "eyJ.jwe"))
	out := propagate(incoming)
	md, _ := metadata.FromOutgoingContext(out)
	if got := md.Get(ContextTokenKey); len(got) != 1 || got[0] != "eyJ.jwe" {
		t.Errorf("propagated token = %v", got)
	}

	// A token the caller set itself wins.
	already := metadata.AppendToOutgoingContext(incoming, ContextTokenKey, "mine")
	md, _ = metadata.FromOutgoingContext(propagate(already))
	if got := md.Get(ContextTokenKey); len(got) != 1 || got[0] != "mine" {
		t.Errorf("token = %v, want the caller's own", got)
	}

	// No incoming token: nothing is added.
	md, ok := metadata.FromOutgoingContext(propagate(context.Background()))
	if ok && len(md.Get(ContextTokenKey)) > 0 {
		t.Errorf("a token appeared from nowhere: %v", md)
	}

	// End to end: a handler at the first hop calls the second service through
	// the client interceptor, and the second hop's checker sees the token.
	h := newHarness(t, Config{})
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return h.listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(UnaryClient()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	secondHop := conformancev1.NewLedgerClient(conn)
	if _, err := secondHop.GetBalance(asWorkforce(incoming, "agent@example.com"), &conformancev1.GetBalanceRequest{}); err != nil {
		t.Fatalf("GetBalance through the client interceptor: %v", err)
	}
	if h.checker.tokens[0] != "eyJ.jwe" {
		t.Errorf("second hop saw token %q, want the first hop's", h.checker.tokens[0])
	}
}

// dial opens another connection to the harness's server, with the given
// client options.
func dial(t *testing.T, h *harness, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	opts = append(opts,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return h.listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	conn, err := grpc.NewClient("passthrough:///bufconn", opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestLookup(t *testing.T) {
	// The fixture service's descriptors are in the global registry through
	// the generated package this test imports.
	ann, err := Lookup(conformancev1.Ledger_GetBalance_FullMethodName)
	if err != nil || ann == nil || ann.Operation != "ledger.balance.read" || ann.Application != "ledger" {
		t.Errorf("Lookup(GetBalance) = %+v, %v", ann, err)
	}
	if ann, err := Lookup("/nobody.v1.Service/Method"); ann != nil || err != nil {
		t.Errorf("an unknown method = %+v, %v; want nil, nil", ann, err)
	}
	if ann, err := Lookup("/conformance.v1/PingRequest"); err == nil || !strings.Contains(err.Error(), "not a method") {
		t.Errorf("a message name = %+v, %v; want a not-a-method error", ann, err)
	}
}

func TestInvalidAnnotationsAreReportedAndDenyPerCall(t *testing.T) {
	h := newHarnessWith(t, Config{}, func(s *grpc.Server) {
		conformancev1.RegisterMalformedServer(s, conformancev1.UnimplementedMalformedServer{})
	})

	err := h.icpt.Verify(h.server)
	if err == nil {
		t.Fatal("Verify passed a server with every malformed annotation")
	}
	for _, fragment := range []string{"NONE is exclusive", "source_details", "usage.subjects", "repeats", "environment", "has_owner", "`usage` is not set"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("Verify did not report %q:\n%v", fragment, err)
		}
	}

	malformed := conformancev1.NewMalformedClient(dial(t, h))
	_, err = malformed.NoneBesideUser(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.PingRequest{})
	if reason, _, ok := Denial(err); !ok || reason != ReasonMisconfigured {
		t.Errorf("a call to a malformed method = %v, want MISCONFIGURED", err)
	}
	if h.checker.calls() != 0 {
		t.Errorf("checker contacted for a malformed method")
	}

	// A call with no actor behind it is the product's own traffic, and an
	// invalid annotation never stops it.
	// The fixture's methods are unimplemented, so a call let through reaches
	// the handler and answers Unimplemented.
	if _, err := malformed.NoneBesideUser(context.Background(), &conformancev1.PingRequest{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a call with no actor to a malformed method = %v, want it let through to the handler", err)
	}
}

func TestAPanickingOwnerLookupIsAFailedCheckNotACrash(t *testing.T) {
	h := newHarness(t, Config{},
		Owner(conformancev1.Ledger_ListTransactions_FullMethodName, func(context.Context, *conformancev1.ListTransactionsRequest) (*accessv1.Owner, error) {
			panic("resolver bug")
		}),
		OwnerLookup(func(context.Context, string) (*accessv1.Owner, error) {
			panic("lookup bug")
		}))
	ctx := asWorkforce(context.Background(), "agent@example.com")

	_, err := h.client.GetStatement(ctx, &conformancev1.GetStatementRequest{AccountId: "acct-7"})
	if reason, _, ok := Denial(err); !ok || reason != ReasonMisconfigured {
		t.Errorf("a panicking lookup: err = %v, want MISCONFIGURED", err)
	}
	_, err = h.client.ListTransactions(ctx, &conformancev1.ListTransactionsRequest{AccountId: "acct-7"})
	if reason, _, ok := Denial(err); !ok || reason != ReasonMisconfigured {
		t.Errorf("a panicking resolver: err = %v, want MISCONFIGURED", err)
	}
	if len(h.checker.failures) != 2 || h.checker.calls() != 0 {
		t.Errorf("failures = %d, checks = %d; want both decided by the client as failures", len(h.checker.failures), h.checker.calls())
	}
	// The server survived both.
	if _, err := h.client.Health(ctx, &conformancev1.HealthRequest{}); err != nil {
		t.Errorf("the server after the panics: %v", err)
	}
}

func TestCloseClosesOnlyAClientTheInterceptorBuilt(t *testing.T) {
	given, err := New(Config{PrincipalResolver: workforce, Checker: &fakeChecker{}})
	if err != nil {
		t.Fatal(err)
	}
	if given.own != nil || given.Close() != nil {
		t.Errorf("an interceptor given a client claims it: %+v", given.own)
	}

	t.Setenv("SENSIVE_MODE", "DISABLED")
	built, err := New(Config{PrincipalResolver: workforce})
	if err != nil {
		t.Fatal(err)
	}
	if built.own == nil || built.checker != built.own {
		t.Fatalf("the interceptor did not keep the client it built")
	}
	if err := built.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestStreamClientInterceptorPropagatesTheContextToken(t *testing.T) {
	h := newHarness(t, Config{})
	incoming := metadata.NewIncomingContext(context.Background(), metadata.Pairs(ContextTokenKey, "eyJ.stream"))
	client := conformancev1.NewLedgerClient(dial(t, h, grpc.WithStreamInterceptor(StreamClient())))

	stream, err := client.Tail(asWorkforce(incoming, "agent@example.com"), &conformancev1.TailRequest{})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if h.checker.tokens[0] != "eyJ.stream" {
		t.Errorf("second hop saw token %q, want the first hop's", h.checker.tokens[0])
	}
}

func TestDenialIgnoresOtherErrors(t *testing.T) {
	for name, err := range map[string]error{
		"a plain error":                     errors.New("boom"),
		"another status code":               status.Error(codes.NotFound, "no such account"),
		"a PermissionDenied from elsewhere": status.Error(codes.PermissionDenied, "not ours"),
	} {
		if reason, token, ok := Denial(err); ok || reason != "" || token != "" {
			t.Errorf("%s: Denial = (%q, %q, %v), want not a Sensive denial", name, reason, token, ok)
		}
	}
}

func TestDeniedMessagesNameTheReason(t *testing.T) {
	for reason, want := range reasonMessages {
		if got := status.Convert(denied(reason, "")).Message(); got != want {
			t.Errorf("%s: message %q, want %q", reason, got, want)
		}
	}
	if got := status.Convert(denied(ReasonDenied, "")).Message(); !strings.Contains(got, "policy") {
		t.Errorf("the checker's denial message %q does not say policy", got)
	}
}

func TestActors(t *testing.T) {
	human := Human("person@example.com").WithSource("workforce-idp")
	if human.Kind != ActorHuman || human.PolicyEmail() != "person@example.com" || human.Source != "workforce-idp" {
		t.Errorf("human = %+v", human)
	}
	own := Agent("https://login.example.com", "bot").WithEmail("bot@example.com")
	if own.Kind != ActorAgent || own.PolicyEmail() != "bot@example.com" {
		t.Errorf("agent with its own email = %+v, policy email %q", own, own.PolicyEmail())
	}
	delegated := Agent("https://login.example.com", "bot").WithEmail("bot@example.com").WithOnBehalfOf("person@example.com")
	if delegated.PolicyEmail() != "person@example.com" {
		t.Errorf("a delegating human should win: %q", delegated.PolicyEmail())
	}
	if none := Agent("https://login.example.com", "bot"); none.PolicyEmail() != "" {
		t.Errorf("an agent with neither should have no policy email: %q", none.PolicyEmail())
	}
}
