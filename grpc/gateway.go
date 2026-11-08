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

import "strings"

// A header matcher in the shape HTTP-to-gRPC gateways take: given a header
// or metadata key, the name to use on the other side and whether to pass it
// at all. grpc-gateway's runtime.HeaderMatcherFunc is this type.
type headerMatcher = func(key string) (string, bool)

// IncomingHeaderMatcher wraps a gateway's incoming header matcher so the
// context token header reaches the service as the metadata key the
// interceptor reads, unrenamed; every other header is left to next. A
// translator that renames headers by default, as grpc-gateway does, would
// otherwise hand the service `grpcgateway-sensive-context-token`.
//
//	runtime.WithIncomingHeaderMatcher(sensivegrpc.IncomingHeaderMatcher(runtime.DefaultHeaderMatcher))
func IncomingHeaderMatcher(next func(key string) (string, bool)) func(key string) (string, bool) {
	return wrap(ContextTokenKey, next)
}

// OutgoingHeaderMatcher wraps a gateway's outgoing header matcher so the
// decision token reaches the client as the `sensive-decision-token` response
// header, unrenamed, on allow and, through the gateway's error handler, on
// deny; every other key is left to next. grpc-gateway's default error
// handler forwards header metadata through this matcher and answers
// PermissionDenied with 403, so a denial needs nothing more.
//
//	runtime.WithOutgoingHeaderMatcher(sensivegrpc.OutgoingHeaderMatcher(runtime.DefaultHeaderMatcher))
func OutgoingHeaderMatcher(next func(key string) (string, bool)) func(key string) (string, bool) {
	return wrap(DecisionTokenKey, next)
}

func wrap(tokenKey string, next headerMatcher) headerMatcher {
	return func(key string) (string, bool) {
		if strings.EqualFold(key, tokenKey) {
			return tokenKey, true
		}
		if next == nil {
			return "", false
		}
		return next(key)
	}
}
