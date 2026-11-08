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

// Package sensivegrpc is Sensive access control for Go gRPC services.
//
// A service declares what each of its methods does with data as Sensive
// options in its own .proto files (the sensive.v1 annotations). The server
// interceptors here read those options off the method descriptor for each
// call, identify the internal actor behind the call with the application's
// principal resolver, build the access-check request and ask the Dynamic
// Access Checker. A method with no annotation is not gated, as a route
// without a `secured()` wrapper is not in the other Sensive SDKs.
//
// The dividing line is whether an internal actor was identified. No actor:
// the call passes untouched and the checker is never contacted. An actor:
// the interceptor asks the client (github.com/sensive-ai/sensive-go/client), and lets
// the call through or refuses it as the client's result says. The client
// alone decides what a check returns, from the mode the environment sets:
// in ENFORCED mode a failure to get a decision denies, so a checker outage
// degrades internal tooling and never the product.
//
// The caller's context token arrives as the call metadata
// ContextTokenKey, and the client interceptors carry it hop to hop.
// The checker's decision token goes back as the response header metadata
// DecisionTokenKey, on allow and deny alike, and on deny also in the
// PermissionDenied status's details, where it survives a chain of callers.
package sensivegrpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"

	sensiveclient "github.com/sensive-ai/sensive-go/client"
	accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ErrorDomain is the domain of the ErrorInfo detail a denial carries.
const ErrorDomain = "sensive.ai"

// The reasons a denial's ErrorInfo detail gives.
const (
	// ReasonDenied is the checker's decision: DENY. The detail's metadata
	// carries the decision token under DecisionTokenKey.
	ReasonDenied = "ACCESS_DENIED"
	// ReasonCheckerUnavailable is a check that could not complete: the
	// checker unreachable, timed out, or answering with an error or without
	// a decision.
	ReasonCheckerUnavailable = "CHECKER_UNAVAILABLE"
	// ReasonUnevaluableActor is an internal agent identified with neither
	// an email of its own nor a human it acts for, which email-keyed
	// policies cannot evaluate.
	ReasonUnevaluableActor = "ACTOR_NOT_EVALUABLE"
	// ReasonMisconfigured is a method whose annotation or owner resolver the
	// runtime could not use: an invalid annotation, a method declaring
	// `has_owner` with no way registered to resolve it, or a resolver or
	// lookup that failed. Verify reports the first two at
	// startup; per call they deny.
	ReasonMisconfigured = "MISCONFIGURED"
)

// Interceptor gates a server's annotated methods. Build one with New, give
// its Unary and Stream interceptors to the server, and call Verify once
// the services are registered.
type Interceptor struct {
	checker  sensiveclient.AccessChecker
	resolver PrincipalResolver
	log      *slog.Logger
	owners   map[string]ownerEntry
	lookup   func(ctx context.Context, key string) (*accessv1.Owner, error)
	// own is the client the interceptor built, which Close closes; nil when
	// the configuration gave one.
	own *sensiveclient.Client
}

type ownerEntry struct {
	input   protoreflect.FullName
	resolve func(ctx context.Context, req any) (*accessv1.Owner, error)
}

// Option configures an Interceptor.
type Option func(*Interceptor) error

// Owner registers the resolver of the owner of the data a method's calls
// touch: the customer whose data it is, looked up where it must be. The
// method is named by its generated full-method constant and the resolver
// is typed by its request message, which Verify checks against the
// method's input. A method declaring `has_owner` needs one; a method not
// declaring it must not have one. The resolver may return nil when a call
// identifies no owner; an error fails the call closed.
func Owner[T proto.Message](fullMethod string, resolve func(ctx context.Context, req T) (*accessv1.Owner, error)) Option {
	var zero T
	input := zero.ProtoReflect().Descriptor().FullName()
	return func(i *Interceptor) error {
		if _, dup := i.owners[fullMethod]; dup {
			return fmt.Errorf("sensive: %s has two owner resolvers", fullMethod)
		}
		i.owners[fullMethod] = ownerEntry{
			input: input,
			resolve: func(ctx context.Context, req any) (*accessv1.Owner, error) {
				typed, ok := req.(T)
				if !ok {
					return nil, fmt.Errorf("request is a %s, resolver wants %s", reflect.TypeOf(req), input)
				}
				return resolve(ctx, typed)
			},
		}
		return nil
	}
}

// OwnerLookup registers the one lookup that resolves an owner from its
// key, for every method that names the field holding it with
// `owner_field`: the interceptor reads that field from the request and
// passes its value here. It may return nil when the key names no one, as
// for a customer that does not exist; an error fails the call closed. An
// empty key is no owner, and the lookup is not called.
func OwnerLookup(lookup func(ctx context.Context, key string) (*accessv1.Owner, error)) Option {
	return func(i *Interceptor) error {
		if i.lookup != nil {
			return errors.New("sensive: OwnerLookup is registered twice")
		}
		i.lookup = lookup
		return nil
	}
}

// New builds an Interceptor from the configuration and the owner
// resolvers.
func New(cfg Config, opts ...Option) (*Interceptor, error) {
	if cfg.PrincipalResolver == nil {
		return nil, errors.New("sensive: a principal resolver is required; it is what keeps the product's traffic outside Sensive's path")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	i := &Interceptor{checker: cfg.Checker, resolver: cfg.PrincipalResolver, log: cfg.Logger, owners: map[string]ownerEntry{}}
	for _, opt := range opts {
		if err := opt(i); err != nil {
			return nil, err
		}
	}
	if i.checker == nil {
		client, err := sensiveclient.FromEnv()
		if err != nil {
			return nil, err
		}
		i.checker, i.own = client, client
	}
	return i, nil
}

// Close closes the client the interceptor built for itself, waiting for the
// LOG_ONLY checks still in flight; a client the configuration gave is its
// owner's to close. Call it when the server has stopped.
func (i *Interceptor) Close() error {
	if i.own == nil {
		return nil
	}
	return i.own.Close()
}

// Unary is the server interceptor for unary methods.
func (i *Interceptor) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := i.decide(ctx, info.FullMethod, req); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream is the server interceptor for streaming methods. A stream is
// checked at open, before any message, so it has no owner.
func (i *Interceptor) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := i.decide(ss.Context(), info.FullMethod, nil); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// decide is the gate: nil lets the call through, an error is the status to
// return instead. Every call to an annotated method with an actor behind it
// is decided by the client, including one whose check cannot be built. A
// call with no actor behind it is the product's own traffic, and passes
// untouched whatever the method's annotation, even an invalid one.
func (i *Interceptor) decide(ctx context.Context, fullMethod string, req any) error {
	ann, err := Lookup(fullMethod)
	if ann == nil && err == nil {
		return nil
	}

	md, _ := metadata.FromIncomingContext(ctx)
	actor := i.resolve(ctx, md, fullMethod)
	if actor == nil {
		return nil
	}
	if err != nil {
		return i.failed(fullMethod, ReasonMisconfigured, fmt.Errorf("the method's annotation is invalid: %w", err))
	}
	email := actor.PolicyEmail()
	if email == "" {
		return i.failed(fullMethod, ReasonUnevaluableActor,
			fmt.Errorf("the agent %s from %s has no identity policies can evaluate", actor.ID, actor.Issuer))
	}

	var owner *accessv1.Owner
	if ann.HasOwner {
		if owner, err = i.resolveOwner(ctx, ann, req); err != nil {
			return i.failed(fullMethod, ReasonMisconfigured, fmt.Errorf("owner resolution failed: %w", err))
		}
	}

	check := ann.Request(email, owner)
	if values := md.Get(ContextTokenKey); len(values) > 0 && values[0] != "" {
		check.ContextToken = proto.String(values[0])
	}
	result := i.checker.CheckAccess(ctx, check)
	if result.DecisionToken != "" {
		if err := grpc.SetHeader(ctx, metadata.Pairs(DecisionTokenKey, result.DecisionToken)); err != nil {
			i.log.Warn("sensive: could not set the decision token header", "method", fullMethod, "error", err)
		}
	}
	switch {
	case result.Allowed():
		return nil
	case result.Source == sensiveclient.SourceChecker:
		i.log.Info("sensive_denied", "method", fullMethod, "operation", ann.Operation, "principal", email)
		return denied(ReasonDenied, result.DecisionToken)
	default:
		i.log.Error("sensive: access check failed; denying", "method", fullMethod, "operation", ann.Operation,
			"source", result.Source, "error", result.Err)
		return denied(ReasonCheckerUnavailable, "")
	}
}

// failed is the gate for a check that could not be built: the client's
// result for it, which denies in ENFORCED mode, with the reason given.
func (i *Interceptor) failed(fullMethod, reason string, cause error) error {
	if i.checker.Failed(cause).Allowed() {
		return nil
	}
	i.log.Error("sensive: denying a call whose check could not be built", "method", fullMethod, "reason", reason, "error", cause)
	return denied(reason, "")
}

// resolveOwner finds the owner of a call to a method declaring has_owner:
// the method's own resolver when one is registered, otherwise the
// owner_field read from the request and passed to the lookup. Both are the
// application's code, so a panic in either is an error, never a crash of
// the server.
func (i *Interceptor) resolveOwner(ctx context.Context, ann *Annotation, req any) (owner *accessv1.Owner, err error) {
	defer func() {
		if r := recover(); r != nil {
			owner, err = nil, fmt.Errorf("resolving the owner panicked: %v", r)
		}
	}()
	if entry, ok := i.owners[ann.FullMethod]; ok {
		return entry.resolve(ctx, req)
	}
	if ann.OwnerField == nil || i.lookup == nil {
		return nil, errors.New("declares has_owner, but no owner resolver is registered and no owner_field names a key for OwnerLookup")
	}
	msg, ok := req.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("request is a %s, not a protobuf message", reflect.TypeOf(req))
	}
	key := msg.ProtoReflect().Get(ann.OwnerField).String()
	if key == "" {
		return nil, nil
	}
	return i.lookup(ctx, key)
}

// resolve runs the principal resolver, treating a panic as no actor.
func (i *Interceptor) resolve(ctx context.Context, md metadata.MD, fullMethod string) (actor *Actor) {
	defer func() {
		if r := recover(); r != nil {
			i.log.Error("sensive: principal resolver panicked; bypassing. A call that fails classification is never gated: fix the resolver", "method", fullMethod, "panic", r)
			actor = nil
		}
	}()
	return i.resolver(ctx, md, fullMethod)
}

// The message each reason's status carries: what a human reading the error
// sees first, before the detail.
var reasonMessages = map[string]string{
	ReasonDenied:             "denied by Sensive access policy",
	ReasonCheckerUnavailable: "Sensive access check could not complete",
	ReasonUnevaluableActor:   "Sensive cannot evaluate this actor's identity",
	ReasonMisconfigured:      "Sensive gating of this method is misconfigured",
}

// denied is the PermissionDenied status a gated call fails with, carrying
// the reason and, for the checker's own denial, the decision token in an
// ErrorInfo detail.
func denied(reason, decisionToken string) error {
	st := status.New(codes.PermissionDenied, reasonMessages[reason])
	info := &errdetails.ErrorInfo{Reason: reason, Domain: ErrorDomain}
	if decisionToken != "" {
		info.Metadata = map[string]string{DecisionTokenKey: decisionToken}
	}
	if detailed, err := st.WithDetails(info); err == nil {
		st = detailed
	}
	return st.Err()
}

// Denial reads the Sensive detail off a status error: the reason, and the
// decision token when the checker denied. ok is false for an error that is
// not a Sensive denial.
func Denial(err error) (reason, decisionToken string, ok bool) {
	st, isStatus := status.FromError(err)
	if !isStatus || st.Code() != codes.PermissionDenied {
		return "", "", false
	}
	for _, d := range st.Details() {
		if info, isInfo := d.(*errdetails.ErrorInfo); isInfo && info.GetDomain() == ErrorDomain {
			return info.GetReason(), info.GetMetadata()[DecisionTokenKey], true
		}
	}
	return "", "", false
}

// Verify checks a server's registered services against the annotations
// and the owner resolvers, once, after registration and before serving:
// every annotation is valid; every method declaring `has_owner` resolves
// its owner one way, from `owner_field` with an OwnerLookup registered or
// with a resolver whose request type is the method's input; no resolver is
// registered for a method that declares no owner; and a registered
// OwnerLookup has a method to serve. It logs one line of
// coverage naming the methods that are not gated, so a gap is seen rather
// than discovered. Returns every problem found, joined.
func (i *Interceptor) Verify(srv *grpc.Server) error {
	var problems []error
	var ungated, gated []string
	seen := map[string]bool{}
	usesLookup := false

	for serviceName, info := range srv.GetServiceInfo() {
		for _, m := range info.Methods {
			fullMethod := "/" + serviceName + "/" + m.Name
			seen[fullMethod] = true
			ann, err := Lookup(fullMethod)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			if ann == nil {
				ungated = append(ungated, fullMethod)
				continue
			}
			gated = append(gated, fullMethod)
			if ann.OwnerField != nil {
				usesLookup = true
			}
			entry, hasResolver := i.owners[fullMethod]
			switch {
			case hasResolver && ann.OwnerField != nil:
				problems = append(problems, fmt.Errorf("sensive: %s names owner_field and also has an owner resolver; keep one", fullMethod))
			case ann.OwnerField != nil && i.lookup == nil:
				problems = append(problems, fmt.Errorf("sensive: %s names owner_field %q but no OwnerLookup is registered", fullMethod, ann.OwnerField.Name()))
			case ann.HasOwner && ann.OwnerField == nil && !hasResolver:
				problems = append(problems, fmt.Errorf("sensive: %s declares has_owner but names no owner_field and has no owner resolver", fullMethod))
			case !ann.HasOwner && hasResolver:
				problems = append(problems, fmt.Errorf("sensive: %s has an owner resolver but does not declare has_owner", fullMethod))
			case hasResolver && entry.input != ann.Input:
				problems = append(problems, fmt.Errorf("sensive: %s: the owner resolver takes %s, the method's request is %s", fullMethod, entry.input, ann.Input))
			}
		}
	}
	if i.lookup != nil && !usesLookup {
		problems = append(problems, errors.New("sensive: an OwnerLookup is registered but no method names an owner_field"))
	}
	for fullMethod := range i.owners {
		if !seen[fullMethod] {
			problems = append(problems, fmt.Errorf("sensive: an owner resolver is registered for %s, which no registered service serves", fullMethod))
		}
	}

	sort.Strings(ungated)
	sort.Strings(gated)
	i.log.Info("sensive_coverage", "gated", len(gated), "ungated", len(ungated), "ungated_methods", ungated)
	return errors.Join(problems...)
}

// UnaryClient propagates an incoming context token to outgoing unary
// calls, so a service that calls another on behalf of the same caller
// carries the context token with it, as it carries the caller's identity.
func UnaryClient() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(propagate(ctx), method, req, reply, cc, opts...)
	}
}

// StreamClient is UnaryClient for streaming calls.
func StreamClient() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(propagate(ctx), desc, cc, method, opts...)
	}
}

// propagate copies the incoming context token to the outgoing
// metadata, unless the caller already set one.
func propagate(ctx context.Context) context.Context {
	if out, ok := metadata.FromOutgoingContext(ctx); ok && len(out.Get(ContextTokenKey)) > 0 {
		return ctx
	}
	in, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	values := in.Get(ContextTokenKey)
	if len(values) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, ContextTokenKey, values[0])
}
