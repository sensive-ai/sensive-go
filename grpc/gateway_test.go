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
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestTheMatchersPassTheTwoTokensUnrenamedAndDeferEverythingElse(t *testing.T) {
	// A stand-in for a gateway's default, which prefixes what it passes.
	prefixing := func(key string) (string, bool) { return "prefixed-" + key, true }

	in := IncomingHeaderMatcher(prefixing)
	if got, ok := in("Sensive-Context-Token"); !ok || got != ContextTokenKey {
		t.Fatalf("incoming context token: %q %v", got, ok)
	}
	if got, _ := in("X-Other"); got != "prefixed-X-Other" {
		t.Fatalf("other incoming headers are next's: %q", got)
	}

	out := OutgoingHeaderMatcher(prefixing)
	if got, ok := out(DecisionTokenKey); !ok || got != DecisionTokenKey {
		t.Fatalf("outgoing decision token: %q %v", got, ok)
	}
	if got, _ := out("x-other"); got != "prefixed-x-other" {
		t.Fatalf("other outgoing keys are next's: %q", got)
	}

	if _, ok := IncomingHeaderMatcher(nil)("X-Other"); ok {
		t.Fatal("with no next, other headers are not passed")
	}
}

// The contract's gRPC half, as a client sees it on the wire: a handler
// that sets the decision token and then denies, as the interceptor does,
// exposes the token among the response headers a plain HTTP/2 client
// reads, not only in the trailers or the status details.
type denyingHealth struct {
	grpc_health_v1.UnimplementedHealthServer
}

func (denyingHealth) Check(ctx context.Context, _ *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	_ = grpc.SetHeader(ctx, metadata.Pairs(DecisionTokenKey, "decision"))
	return nil, status.Error(codes.PermissionDenied, "denied")
}

func TestADeniedCallCarriesTheDecisionTokenAsAResponseHeader(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	grpc_health_v1.RegisterHealthServer(server, denyingHealth{})
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	client := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(_ context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	}}
	req, _ := http.NewRequest(http.MethodPost, "http://"+lis.Addr().String()+"/grpc.health.v1.Health/Check",
		strings.NewReader("\x00\x00\x00\x00\x00")) // an empty message
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := resp.Header.Get(DecisionTokenKey); got != "decision" {
		t.Fatalf("decision token header %q; headers %v", got, resp.Header)
	}
	if got := resp.Trailer.Get("Grpc-Status"); got != "7" {
		t.Fatalf("grpc-status %q, want 7 (PermissionDenied)", got)
	}
}
