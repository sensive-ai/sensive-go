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

// Package sensiveclient is the Go client of the Sensive Dynamic Access Checker.
//
// The client calls the checker over gRPC. Its messages and stubs are
// generated from the checker's protos into gen/: the request a caller builds
// is the API's own AccessCheckRequest (gen/sensive/access/v1), in the data
// taxonomy's vocabulary (gen/sensive/taxonomy/v1), so it is the one every
// Sensive SDK sends.
//
// The client keeps the contract every Sensive SDK keeps with the environment
// it runs in (README.md, "Configuration"): its settings come from the
// environment and nowhere else, and it alone decides what a check returns
// when the mode says checks are not enforced or the checker gives no
// decision. A check always returns ALLOW or DENY, so every SDK built on the
// client, and every service that calls it, decides the same way.
package sensiveclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
	taxonomyv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/taxonomy/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// AccessChecker is what a Sensive runtime asks for a decision: the one
// method of Client it needs, as an interface so a test can stand in for
// the client.
type AccessChecker interface {
	CheckAccess(ctx context.Context, req *accessv1.AccessCheckRequest) Result
	Failed(cause error) Result
}

// Source says where a check's result came from. It is for logs and
// telemetry: a caller acts on the result's decision alone.
type Source string

const (
	// SourceChecker is the checker's own decision, with its decision token.
	SourceChecker Source = "CHECKER"
	// SourceTimeout is DENY: the checker did not answer within the timeout.
	SourceTimeout Source = "TIMEOUT"
	// SourceFailure is DENY: the checker could not be reached, answered
	// with an error, or answered with no decision.
	SourceFailure Source = "FAILURE"
	// SourceMode is ALLOW: the mode says checks are not enforced.
	SourceMode Source = "MODE"
)

// Result is what a check returns: ALLOW or DENY, and nothing else.
type Result struct {
	// Decision is ALLOW or DENY.
	Decision accessv1.AccessDecision_Value
	// DecisionToken is the checker's signed decision token, present only
	// on the checker's own decision. A client shows it to the user so they
	// can justify a denial and retry.
	DecisionToken string
	// Source is where the decision came from.
	Source Source
	// Err is the failure behind a TIMEOUT or FAILURE result, for logging.
	Err error
}

// Allowed reports whether the result is ALLOW.
func (r Result) Allowed() bool {
	return r.Decision == accessv1.AccessDecision_ALLOW
}

// notEnforced is the result of every check in LOG_ONLY and DISABLED mode.
var notEnforced = Result{Decision: accessv1.AccessDecision_ALLOW, Source: SourceMode}

// ErrNoDecision is the failure behind a result whose checker answered
// without a decision or a verdict: the checker, or something in front of
// it, answered without the documented response.
var ErrNoDecision = errors.New("dynamic access checker: the answer carried no decision")

// logOnlyInFlight bounds the LOG_ONLY checks in flight at once. A check
// with no room is not sent: the caller never waits on Sensive.
const logOnlyInFlight = 256

// Client is a client of one Dynamic Access Checker, configured by the
// environment. Create it once and share it: it holds one connection, which
// gRPC multiplexes, and its methods are safe to call concurrently. Close
// releases the connection.
type Client struct {
	settings      settings
	conn          *grpc.ClientConn // nil in DISABLED mode
	authorization accessv1.AuthorizationServiceClient

	inFlight   chan struct{}
	background sync.WaitGroup
	// mu guards closed, so a background check never starts once Close has
	// begun waiting for the others.
	mu     sync.Mutex
	closed bool
}

var _ AccessChecker = (*Client)(nil)

// FromEnv returns the client the environment configures (environment.go):
// the checker in SENSIVE_DAC_URL, in the mode SENSIVE_MODE names. An
// environment it cannot read is an error naming the variable
// (*ConfigError). It logs once what it read.
//
// The options are gRPC's, for the connection only: a private certificate
// authority, interceptors, keepalive. They are applied after the client's
// own, so one that sets the transport credentials replaces the client's;
// they never change where the client connects. The connection is made on
// the first check.
func FromEnv(opts ...grpc.DialOption) (*Client, error) {
	s, err := settingsFrom(os.LookupEnv)
	if err != nil {
		return nil, err
	}
	client, err := newClient(s, opts...)
	if err != nil {
		return nil, err
	}
	slog.Info("sensive: configured", s.logAttrs()...)
	return client, nil
}

// newClient is the client for settings already resolved.
func newClient(s settings, opts ...grpc.DialOption) (*Client, error) {
	client := &Client{settings: s, inFlight: make(chan struct{}, logOnlyInFlight)}
	if s.mode == ModeDisabled {
		return client, nil
	}
	transport := insecure.NewCredentials()
	if s.secure {
		transport = credentials.NewClientTLSFromCert(nil, "")
	}
	dial := []grpc.DialOption{grpc.WithTransportCredentials(transport)}
	dial = append(dial, opts...)
	conn, err := grpc.NewClient(s.target, dial...)
	if err != nil {
		return nil, fmt.Errorf("sensive client: %w", err)
	}
	client.conn = conn
	client.authorization = accessv1.NewAuthorizationServiceClient(conn)
	return client, nil
}

// Close waits for the LOG_ONLY checks still in flight, each bounded by the
// timeout, and releases the client's connection. A LOG_ONLY check asked for
// once Close has begun is not sent; an ENFORCED one fails, and is DENY.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.background.Wait()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// CheckAccess asks for a decision on one request, and always returns ALLOW
// or DENY. The client sends the request with the environment's
// SENSIVE_ENVIRONMENT and the mode, whatever the request carried. The
// context token, when the caller has one, is the request's
// ContextToken, opaque to the client.
//
//   - ENFORCED: the checker's decision, with its token. A check that does
//     not get one within the timeout, or fails, is DENY.
//   - LOG_ONLY: ALLOW, at once. The check is sent in the background, and
//     the checker records what it would have decided.
//   - DISABLED: ALLOW, at once. Nothing is sent.
func (c *Client) CheckAccess(ctx context.Context, req *accessv1.AccessCheckRequest) Result {
	switch c.settings.mode {
	case ModeDisabled:
		return notEnforced
	case ModeLogOnly:
		c.sendInBackground(c.stamped(req, accessv1.Mode_LOG_ONLY))
		return notEnforced
	}
	ctx, cancel := context.WithTimeout(ctx, c.settings.timeout)
	defer cancel()
	resp, err := c.authorization.CheckAccess(ctx, c.stamped(req, accessv1.Mode_ENFORCED))
	return decided(resp, err)
}

// stamped is the request as it is sent: a copy, carrying the environment's
// environment and the mode.
func (c *Client) stamped(req *accessv1.AccessCheckRequest, mode accessv1.Mode_Value) *accessv1.AccessCheckRequest {
	out := proto.Clone(req).(*accessv1.AccessCheckRequest)
	if out.Usage == nil {
		out.Usage = &taxonomyv1.UsageDescriptors{}
	}
	out.Usage.Environment = c.settings.environment.Enum()
	out.Mode = mode.Enum()
	return out
}

// sendInBackground sends a LOG_ONLY check without the caller waiting on
// any of it: the name lookup, the connection and the call all happen on
// the goroutine. A check with no room in flight, or asked for once Close
// has begun, is not sent.
func (c *Client) sendInBackground(req *accessv1.AccessCheckRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.inFlight <- struct{}{}:
	default:
		return
	}
	c.background.Add(1)
	go func() {
		defer func() {
			<-c.inFlight
			c.background.Done()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), c.settings.timeout)
		defer cancel()
		_, _ = c.authorization.CheckAccess(ctx, req)
	}()
}

// decided is the result of an ENFORCED check: the checker's decision, or
// DENY for anything else.
func decided(resp *accessv1.AccessCheckResponse, err error) Result {
	deny := func(source Source, err error) Result {
		return Result{Decision: accessv1.AccessDecision_DENY, Source: source, Err: err}
	}
	switch {
	case status.Code(err) == codes.DeadlineExceeded:
		return deny(SourceTimeout, fmt.Errorf("sensive client: check access: %w", err))
	case err != nil:
		return deny(SourceFailure, fmt.Errorf("sensive client: check access: %w", err))
	}
	switch resp.GetResult() {
	case accessv1.AccessDecision_ALLOW, accessv1.AccessDecision_DENY:
		return Result{Decision: resp.GetResult(), DecisionToken: resp.GetDecisionToken(), Source: SourceChecker}
	default:
		return deny(SourceFailure, fmt.Errorf("%w: the result is %s", ErrNoDecision, resp.GetResult()))
	}
}

// Failed is the result of a check the caller could not build, such as one
// whose owner lookup failed: DENY in ENFORCED mode, and ALLOW in LOG_ONLY
// and DISABLED, as for every check. Nothing is sent. The cause is the
// result's Err in ENFORCED mode.
func (c *Client) Failed(cause error) Result {
	if c.settings.mode != ModeEnforced {
		return notEnforced
	}
	return Result{Decision: accessv1.AccessDecision_DENY, Source: SourceFailure, Err: cause}
}

// OwnerID is the owner of the data a request touches, by the backend's own
// identifier: the preferred form.
func OwnerID(id string) *accessv1.Owner {
	return &accessv1.Owner{Id: proto.String(id)}
}

// OwnerEmail is the owner of the data a request touches, by email: the
// form for a backend with no identifier of its own.
func OwnerEmail(email string) *accessv1.Owner {
	return &accessv1.Owner{Email: proto.String(email)}
}
