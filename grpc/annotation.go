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
	"errors"
	"fmt"
	"slices"
	"strings"

	accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
	taxonomyv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/taxonomy/v1"
	sensivev1 "github.com/sensive-ai/sensive-go/grpc/gen/sensive/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Metadata keys the runtime reads and writes, the gRPC spelling of the
// headers the HTTP SDKs use.
const (
	// ContextTokenKey carries the caller's context token, opaque to the
	// service, into a call and from hop to hop.
	ContextTokenKey = "sensive-context-token"
	// DecisionTokenKey carries the checker's signed decision token back as
	// response header metadata, on allow and deny alike, so a client can
	// tell "checked" from "no Sensive coverage".
	DecisionTokenKey = "sensive-decision-token"
)

// Annotation is what a method's Sensive options say, validated: everything
// the access check needs except the facts of one call.
type Annotation struct {
	// FullMethod is the method in gRPC's spelling, `/pkg.Service/Method`.
	FullMethod string
	// Input is the request message's full name, which an owner resolver's
	// request type must match.
	Input protoreflect.FullName
	// Streaming is set for a client- or server-streaming method, which is
	// gated at open and has no request message to resolve an owner from.
	Streaming bool

	// Application and Categories come from the service's option, or the
	// file's default.
	Application string
	Categories  []string
	// Operation is the operation's name: the option's, or the method's full
	// name with dots.
	Operation string
	// Capability is what the operation does with data.
	Capability *taxonomyv1.CapabilityDescriptors
	// Subjects is who the data is about, declared by every annotated method
	// as every SDK requires; SourceDetails is who produced it, present
	// exactly when the sources include the application's user.
	Subjects      []taxonomyv1.Subject_Value
	SourceDetails []taxonomyv1.SourceDetail_Value
	// HasOwner says the request identifies the owner of the data it
	// touches, resolved from OwnerField or by a resolver registered for the
	// method.
	HasOwner bool
	// OwnerField is the request field that holds the owner's key, which the
	// interceptor reads and passes to the service's owner lookup; nil when
	// the method resolves its owner another way, or has none.
	OwnerField protoreflect.FieldDescriptor
}

// Request is the access check for one call of the annotated method: the
// actor's policy email and the owner the resolver found, if any. The
// client adds the environment, from SENSIVE_ENVIRONMENT.
func (a *Annotation) Request(email string, owner *accessv1.Owner) *accessv1.AccessCheckRequest {
	return &accessv1.AccessCheckRequest{
		Principal:   &accessv1.Principal{Email: proto.String(email)},
		Application: proto.String(a.Application),
		Operation:   proto.String(a.Operation),
		Categories:  slices.Clone(a.Categories),
		Capability:  proto.CloneOf(a.Capability),
		Usage: &taxonomyv1.UsageDescriptors{
			Subjects:      slices.Clone(a.Subjects),
			SourceDetails: slices.Clone(a.SourceDetails),
		},
		Owner: owner,
	}
}

// Lookup finds the annotation of a method by its gRPC full name, in the
// descriptors the generated code registered. It returns nil for a method
// with no `operation` option, which is not gated, and for a method the
// registry does not know, which a server cannot be serving through
// generated code. An invalid annotation is an error.
func Lookup(fullMethod string) (*Annotation, error) {
	name := fullNameOf(fullMethod)
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	if errors.Is(err, protoregistry.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sensive: %s: %w", fullMethod, err)
	}
	method, ok := desc.(protoreflect.MethodDescriptor)
	if !ok {
		return nil, fmt.Errorf("sensive: %s names a %T, not a method", fullMethod, desc)
	}
	return annotationOf(method)
}

// fullNameOf turns gRPC's `/pkg.Service/Method` into the protobuf full
// name `pkg.Service.Method`.
func fullNameOf(fullMethod string) protoreflect.FullName {
	return protoreflect.FullName(strings.ReplaceAll(strings.TrimPrefix(fullMethod, "/"), "/", "."))
}

// grpcMethodOf turns a method descriptor's full name into gRPC's spelling.
func grpcMethodOf(method protoreflect.MethodDescriptor) string {
	return "/" + string(method.Parent().FullName()) + "/" + string(method.Name())
}

// annotationOf reads and validates one method's options. Nil, nil for a
// method with no `operation` option.
func annotationOf(method protoreflect.MethodDescriptor) (*Annotation, error) {
	methodOpts := method.Options()
	if methodOpts == nil || !proto.HasExtension(methodOpts, sensivev1.E_Operation) {
		return nil, nil
	}
	op, _ := proto.GetExtension(methodOpts, sensivev1.E_Operation).(*sensivev1.Operation)
	full := grpcMethodOf(method)
	fail := func(format string, args ...any) error {
		return fmt.Errorf("sensive: %s: %s", full, fmt.Sprintf(format, args...))
	}

	ann := &Annotation{
		FullMethod: full,
		Input:      method.Input().FullName(),
		Streaming:  method.IsStreamingClient() || method.IsStreamingServer(),
		Operation:  strings.TrimSpace(op.GetName()),
		HasOwner:   op.GetHasOwner(),
	}
	if ann.Operation == "" {
		ann.Operation = string(method.FullName())
	}

	// The application: the service's, else the file's default.
	service, _ := method.Parent().(protoreflect.ServiceDescriptor)
	var app *sensivev1.Application
	if o := service.Options(); o != nil && proto.HasExtension(o, sensivev1.E_Application) {
		app, _ = proto.GetExtension(o, sensivev1.E_Application).(*sensivev1.Application)
	} else if o := service.ParentFile().Options(); o != nil && proto.HasExtension(o, sensivev1.E_DefaultApplication) {
		app, _ = proto.GetExtension(o, sensivev1.E_DefaultApplication).(*sensivev1.Application)
	}
	if app == nil || strings.TrimSpace(app.GetName()) == "" {
		return nil, fail("no application: the service has no `application` option and its file no `default_application`")
	}
	ann.Application = app.GetName()
	ann.Categories = app.GetCategories()

	// The capability descriptors: every set non-empty. An SDK route
	// declares every dimension, so each is required here although the wire
	// lets a caller omit what it does not know.
	cap := op.GetCapability()
	if cap == nil {
		return nil, fail("`capability` is not set")
	}
	if err := checkSet(cap.GetSources()); err != nil {
		return nil, fail("capability.sources %v", err)
	}
	if err := checkSet(cap.GetSemanticRoles()); err != nil {
		return nil, fail("capability.semantic_roles %v", err)
	}
	if err := checkSet(cap.GetActionTypes()); err != nil {
		return nil, fail("capability.action_types %v", err)
	}
	ann.Capability = proto.CloneOf(cap)

	// The usage descriptors: declared per method except the environment.
	// Subjects are required, as every SDK requires them: a route declares
	// who its data is about, and only a proxied application nobody has
	// annotated sends none.
	usage := op.GetUsage()
	if usage == nil {
		return nil, fail("`usage` is not set")
	}
	if usage.Environment != nil {
		return nil, fail("usage.environment is set on the method; the environment is the deployment's configuration")
	}
	subjects := usage.GetSubjects()
	if err := checkSet(subjects); err != nil {
		return nil, fail("usage.subjects %v", err)
	}
	if len(subjects) > 1 && slices.Contains(subjects, taxonomyv1.Subject_NONE) {
		return nil, fail("usage.subjects: NONE is exclusive and cannot appear beside another subject")
	}
	ann.Subjects = slices.Clone(subjects)
	hasUserSource := slices.Contains(cap.GetSources(), taxonomyv1.Source_APPLICATION_USER)
	if details := usage.GetSourceDetails(); len(details) > 0 {
		if !hasUserSource {
			return nil, fail("usage.source_details is set but capability.sources has no APPLICATION_USER, whose subcategory it is")
		}
		if err := checkSet(details); err != nil {
			return nil, fail("usage.source_details %v", err)
		}
		ann.SourceDetails = slices.Clone(details)
	} else if hasUserSource {
		return nil, fail("capability.sources has APPLICATION_USER but usage.source_details says nothing about who produced the data")
	}

	// The owner's key, when a request field holds it: a singular string
	// field of the request message, which implies has_owner.
	if name := strings.TrimSpace(op.GetOwnerField()); name != "" {
		field := method.Input().Fields().ByName(protoreflect.Name(name))
		switch {
		case field == nil:
			return nil, fail("owner_field %q is not a field of %s", name, method.Input().FullName())
		case field.Kind() != protoreflect.StringKind || field.IsList() || field.IsMap():
			return nil, fail("owner_field %q is not a singular string field of %s", name, method.Input().FullName())
		}
		ann.OwnerField = field
		ann.HasOwner = true
	}

	if ann.HasOwner && ann.Streaming {
		return nil, fail("has_owner or owner_field is set on a streaming method, which has no request message to resolve an owner from at open")
	}
	return ann, nil
}

// checkSet holds one of an annotation's sets to what every SDK requires of
// a route's: at least one value, each a value the taxonomy defines, none
// repeated.
func checkSet[E interface {
	~int32
	protoreflect.Enum
}](values []E) error {
	if len(values) == 0 {
		return errors.New("is empty; every set declares at least one value")
	}
	seen := map[E]bool{}
	for _, v := range values {
		value := v.Descriptor().Values().ByNumber(v.Number())
		switch {
		case value == nil:
			return fmt.Errorf("has the unknown value %d", v.Number())
		case v.Number() == 0:
			return fmt.Errorf("has %s, which names no value", value.Name())
		case seen[v]:
			return fmt.Errorf("repeats %s", value.Name())
		}
		seen[v] = true
	}
	return nil
}
