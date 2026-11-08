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
	"log/slog"

	sensiveclient "github.com/sensive-ai/sensive-go/client"
	"google.golang.org/grpc/metadata"
)

// ActorKind says what an Actor is: a workforce human or an internal AI
// agent.
type ActorKind string

const (
	// ActorHuman is an internal workforce human.
	ActorHuman ActorKind = "human"
	// ActorAgent is an internal AI agent, with or without an identity of its
	// own that policies can evaluate.
	ActorAgent ActorKind = "agent"
)

// Actor is an internal actor positively identified behind a call: the
// population Sensive gates. Everything else, the product's end users and
// service-to-service traffic, stays outside Sensive's path.
type Actor struct {
	Kind ActorKind
	// Email is a human's workforce email, matching the IdP identity policies
	// key on, or an agent's own email identity when it has one.
	Email string
	// ID and Issuer identify an agent: a stable identifier and the trust
	// domain that issued it.
	ID, Issuer string
	// OnBehalfOf is the workforce human an agent acts for, when the call
	// carries a verified delegation; access is then evaluated as that human.
	OnBehalfOf string
	// Source says which signal identified the actor. For observability
	// only, never for the decision.
	Source string
}

// Human is a workforce human identified by their email.
func Human(email string) *Actor {
	return &Actor{Kind: ActorHuman, Email: email}
}

// Agent is an internal AI agent identified by its issuer and id.
func Agent(issuer, id string) *Actor {
	return &Actor{Kind: ActorAgent, Issuer: issuer, ID: id}
}

// WithEmail gives an agent its own email identity.
func (a *Actor) WithEmail(email string) *Actor {
	a.Email = email
	return a
}

// WithOnBehalfOf records the workforce human an agent acts for.
func (a *Actor) WithOnBehalfOf(email string) *Actor {
	a.OnBehalfOf = email
	return a
}

// WithSource records which signal identified the actor.
func (a *Actor) WithSource(source string) *Actor {
	a.Source = source
	return a
}

// PolicyEmail is the email policies are evaluated against: a human's own,
// or the delegating human's for an agent acting on someone's behalf, else
// the agent's own. Empty for an agent with neither, which is recognized as
// gateable but cannot be evaluated by email-keyed policies, and is denied.
func (a *Actor) PolicyEmail() string {
	if a.Kind == ActorHuman {
		return a.Email
	}
	if a.OnBehalfOf != "" {
		return a.OnBehalfOf
	}
	return a.Email
}

// PrincipalResolver answers one question about an incoming call: is there
// an internal actor behind it that Sensive should gate, and who? It returns
// the actor, or nil for everything else, on which the interceptor bypasses
// and the checker is never contacted.
//
// Rules for implementations, the same as every Sensive SDK's: return an actor
// only on positive recognition of an internal principal, from a verified
// source such as claims an authentication interceptor already checked;
// never classify on the absence of a field; stay synchronous and do no
// I/O, since the resolver runs on every call including all of the
// product's traffic. A panic is recovered and treated as no actor.
type PrincipalResolver func(ctx context.Context, md metadata.MD, fullMethod string) *Actor

// Config is what code configures: the principal resolver, and optionally
// the client the interceptor asks and its logger. Everything about the
// deployment, the checker, the environment, the timeout and the mode, is
// the client's, read from the environment every Sensive SDK reads
// (the client module's README, "Configuration"). The application and its
// categories are declared in the service's own proto options, with the
// rest of the annotation.
type Config struct {
	// PrincipalResolver classifies each call. Required.
	PrincipalResolver PrincipalResolver
	// Checker is the client the interceptor asks: a sensiveclient.Client from
	// sensiveclient.FromEnv, shared with other code or built with gRPC dial options
	// of its own, or a stand-in for tests. Unset, the interceptor builds
	// one with sensiveclient.FromEnv.
	Checker sensiveclient.AccessChecker
	// Logger receives the interceptor's log lines. Defaults to slog's.
	Logger *slog.Logger
}

// FromEnv builds the interceptor from the principal resolver and the
// options, such as the owner lookup, with the client the environment
// configures (sensiveclient.FromEnv): an environment the client cannot read is an
// error naming the variable.
func FromEnv(resolver PrincipalResolver, opts ...Option) (*Interceptor, error) {
	return New(Config{PrincipalResolver: resolver}, opts...)
}
