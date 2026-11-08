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

package sensiveclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
	taxonomyv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/taxonomy/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The request every SDK's documentation builds, with a context token
// and no environment, which the client adds.
func sampleRequest() *accessv1.AccessCheckRequest {
	return &accessv1.AccessCheckRequest{
		Principal:   &accessv1.Principal{Email: proto.String("agent@example.com")},
		Application: proto.String("yobank-support-api"),
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
		Owner:        OwnerID("cust_123"),
		ContextToken: proto.String("jwe-token"),
	}
}

// arrival is one check as the fake checker received it.
type arrival struct {
	request *accessv1.AccessCheckRequest
}

// checker is a Dynamic Access Checker that reports each check as it
// arrives and answers as the test says: answer is called once the check is
// let go, which is at once unless hold is set, until release is closed.
type checker struct {
	accessv1.UnimplementedAuthorizationServiceServer

	answer  func(ctx context.Context) (*accessv1.AccessCheckResponse, error)
	hold    bool
	release chan struct{}
	arrived chan arrival
}

func newChecker(answer func(ctx context.Context) (*accessv1.AccessCheckResponse, error)) *checker {
	return &checker{answer: answer, release: make(chan struct{}), arrived: make(chan arrival, 1024)}
}

func (c *checker) CheckAccess(ctx context.Context, req *accessv1.AccessCheckRequest) (*accessv1.AccessCheckResponse, error) {
	c.arrived <- arrival{request: req}
	if c.hold {
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.answer(ctx)
}

func answering(resp *accessv1.AccessCheckResponse, err error) func(context.Context) (*accessv1.AccessCheckResponse, error) {
	return func(context.Context) (*accessv1.AccessCheckResponse, error) { return resp, err }
}

func allow() *accessv1.AccessCheckResponse {
	return &accessv1.AccessCheckResponse{Result: accessv1.AccessDecision_ALLOW.Enum(), DecisionToken: proto.String("decision-token")}
}

// serve runs the checker on a loopback port, over TLS when creds are given,
// and returns its URL.
func serve(t *testing.T, c *checker, creds credentials.TransportCredentials) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var opts []grpc.ServerOption
	scheme := "http"
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
		scheme = "https"
	}
	server := grpc.NewServer(opts...)
	accessv1.RegisterAuthorizationServiceServer(server, c)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return scheme + "://" + lis.Addr().String() + "/api/dac"
}

// clientFor is the client an environment configures.
func clientFor(t *testing.T, env map[string]string, opts ...grpc.DialOption) *Client {
	t.Helper()
	s, err := settingsFrom(lookupIn(env))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	client, err := newClient(s, opts...)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func lookupIn(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func TestTheTargetIsTheURLsHostAndPort(t *testing.T) {
	for in, want := range map[string]struct {
		target string
		secure bool
	}{
		"https://sensive.example.com/api/dac":  {"sensive.example.com:443", true},
		"https://sensive.example.com:8443":     {"sensive.example.com:8443", true},
		"http://localhost:3010/api/dac":        {"localhost:3010", false},
		"http://localhost":                     {"localhost:80", false},
		"https://sensive.example.com/api/dac/": {"sensive.example.com:443", true},
	} {
		target, secure, err := targetOf(in)
		if err != nil || target != want.target || secure != want.secure {
			t.Errorf("targetOf(%q) = %q, %v, %v; want %q, %v", in, target, secure, err, want.target, want.secure)
		}
	}
}

func TestARefusalNamesItsVariable(t *testing.T) {
	_, err := settingsFrom(lookupIn(map[string]string{"SENSIVE_MODE": "OFF"}))
	var refused *ConfigError
	if !errors.As(err, &refused) || refused.Variable != "SENSIVE_MODE" || !strings.Contains(err.Error(), "SENSIVE_MODE") {
		t.Fatalf("err = %v, want a ConfigError naming SENSIVE_MODE", err)
	}
}

func TestTheRequestIsSentAsGivenWithTheEnvironmentAndMode(t *testing.T) {
	c := newChecker(answering(allow(), nil))
	client := clientFor(t, map[string]string{"SENSIVE_DAC_URL": serve(t, c, nil), "SENSIVE_ENVIRONMENT": "TEST_DATA"})
	req := sampleRequest()

	client.CheckAccess(context.Background(), req)
	got := (<-c.arrived).request

	want := sampleRequest()
	want.Usage.Environment = taxonomyv1.Environment_TEST_DATA.Enum()
	want.Mode = accessv1.Mode_ENFORCED.Enum()
	if !proto.Equal(got, want) {
		t.Errorf("the checker received %v", got)
	}
	if !proto.Equal(req, sampleRequest()) {
		t.Errorf("the caller's request was changed: %v", req)
	}
}

func TestTheClientChecksOverTLS(t *testing.T) {
	serverTLS, roots := selfSigned(t)
	c := newChecker(answering(allow(), nil))
	checkerURL := serve(t, c, credentials.NewTLS(serverTLS))
	client := clientFor(t, map[string]string{"SENSIVE_DAC_URL": checkerURL},
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: roots})))

	if result := client.CheckAccess(context.Background(), sampleRequest()); result.Source != SourceChecker {
		t.Fatalf("check over TLS: %+v", result)
	}
}

func TestAFailureKeepsItsCause(t *testing.T) {
	c := newChecker(answering(nil, status.Error(codes.Unauthenticated, "refused")))
	client := clientFor(t, map[string]string{"SENSIVE_DAC_URL": serve(t, c, nil)})

	result := client.CheckAccess(context.Background(), sampleRequest())
	if result.Allowed() || result.Source != SourceFailure || status.Code(result.Err) != codes.Unauthenticated {
		t.Fatalf("result = %+v, want DENY from a failure carrying the checker's status", result)
	}
}

func TestACancelledCallIsDenied(t *testing.T) {
	client := clientFor(t, map[string]string{"SENSIVE_DAC_URL": serve(t, newChecker(answering(allow(), nil)), nil)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := client.CheckAccess(ctx, sampleRequest()); result.Allowed() || result.Source != SourceFailure {
		t.Errorf("result = %+v, want DENY from a failure", result)
	}
}

func TestLogOnlyChecksInFlightAreBounded(t *testing.T) {
	c := newChecker(answering(allow(), nil))
	c.hold = true
	client := clientFor(t, map[string]string{"SENSIVE_DAC_URL": serve(t, c, nil), "SENSIVE_MODE": "LOG_ONLY"})

	for range logOnlyInFlight + 10 {
		if result := client.CheckAccess(context.Background(), sampleRequest()); !result.Allowed() {
			t.Fatalf("result = %+v", result)
		}
	}
	for range logOnlyInFlight {
		<-c.arrived
	}
	close(c.release)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.arrived:
		t.Error("a check beyond the bound was sent")
	default:
	}
}

func TestCloseRacesNoBackgroundCheck(t *testing.T) {
	c := newChecker(answering(allow(), nil))
	client := clientFor(t, map[string]string{"SENSIVE_DAC_URL": serve(t, c, nil), "SENSIVE_MODE": "LOG_ONLY"})

	// Checks asked for while Close runs, as during a graceful shutdown:
	// none may panic, and none is sent once Close has begun.
	var callers sync.WaitGroup
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for range 50 {
				client.CheckAccess(context.Background(), sampleRequest())
			}
		}()
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	sentBefore := len(c.arrived)
	callers.Wait()
	if after := len(c.arrived); after != sentBefore {
		t.Errorf("%d checks were sent after Close returned", after-sentBefore)
	}
}

func TestOwnerHelpers(t *testing.T) {
	if o := OwnerID("cust_1"); o.GetId() != "cust_1" || o.Email != nil {
		t.Errorf("OwnerID = %v", o)
	}
	if o := OwnerEmail("a@example.com"); o.GetEmail() != "a@example.com" || o.Id != nil {
		t.Errorf("OwnerEmail = %v", o)
	}
}

// selfSigned is a server certificate for 127.0.0.1, and a pool that trusts
// it.
func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "checker"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, roots
}
