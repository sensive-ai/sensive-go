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
	"encoding/json"
	"os"
	"testing"

	sensiveclient "github.com/sensive-ai/sensive-go/client"
	accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
	conformancev1 "github.com/sensive-ai/sensive-go/grpc/internal/conformancepb/conformance/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The environment contract's cases, which the module ships in testdata.
// The client decides every check; what this runtime must do is let a call
// through or refuse it as the client's result says, with the decision token
// on the response when there is one.
type contractResult struct {
	Decision string `json:"decision"`
	Token    bool   `json:"token"`
	Source   string `json:"source"`
}

func readContract(t *testing.T) (checks, failures []struct {
	Name   string         `json:"name"`
	Result contractResult `json:"result"`
}) {
	t.Helper()
	data, err := os.ReadFile("testdata/environment.json")
	if err != nil {
		t.Fatalf("the corpus (run the generate script): %v", err)
	}
	var corpus struct {
		Checks []struct {
			Name   string         `json:"name"`
			Result contractResult `json:"result"`
		} `json:"checks"`
		Failures []struct {
			Name   string         `json:"name"`
			Result contractResult `json:"result"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	return corpus.Checks, corpus.Failures
}

// resultChecker is a client that returns one result for every check.
type resultChecker struct{ result sensiveclient.Result }

func (r resultChecker) CheckAccess(context.Context, *accessv1.AccessCheckRequest) sensiveclient.Result {
	return r.result
}

func (r resultChecker) Failed(error) sensiveclient.Result { return r.result }

func asResult(c contractResult) sensiveclient.Result {
	result := sensiveclient.Result{
		Decision: accessv1.AccessDecision_Value(accessv1.AccessDecision_Value_value[c.Decision]),
		Source:   sensiveclient.Source(c.Source),
	}
	if c.Token {
		result.DecisionToken = "decision-token"
	}
	return result
}

func TestTheClientsResultDecidesTheCall(t *testing.T) {
	checks, _ := readContract(t)
	for _, tc := range checks {
		t.Run(tc.Name, func(t *testing.T) {
			h := newHarness(t, Config{Checker: resultChecker{asResult(tc.Result)}})
			var header metadata.MD
			_, err := h.client.GetBalance(asWorkforce(context.Background(), "agent@example.com"), &conformancev1.GetBalanceRequest{}, grpc.Header(&header))

			if tc.Result.Decision == "ALLOW" {
				if err != nil {
					t.Fatalf("an ALLOW was refused: %v", err)
				}
			} else {
				want := ReasonCheckerUnavailable
				if tc.Result.Source == string(sensiveclient.SourceChecker) {
					want = ReasonDenied
				}
				if reason, _, ok := Denial(err); status.Code(err) != codes.PermissionDenied || !ok || reason != want {
					t.Fatalf("err = %v, want PermissionDenied with %s", err, want)
				}
			}
			if got := header.Get(DecisionTokenKey); (len(got) == 1) != tc.Result.Token {
				t.Errorf("decision token header = %v, want one: %v", got, tc.Result.Token)
			}
		})
	}
}

func TestTheClientsResultDecidesACheckThatCouldNotBeBuilt(t *testing.T) {
	_, failures := readContract(t)
	for _, tc := range failures {
		t.Run(tc.Name, func(t *testing.T) {
			// An agent with no identity policies can evaluate is a check the
			// runtime cannot build.
			h := newHarness(t, Config{
				Checker: resultChecker{asResult(tc.Result)},
				PrincipalResolver: func(context.Context, metadata.MD, string) *Actor {
					return Agent("https://login.example.com", "copilot")
				},
			})
			_, err := h.client.GetBalance(context.Background(), &conformancev1.GetBalanceRequest{})
			if tc.Result.Decision == "ALLOW" {
				if err != nil {
					t.Fatalf("an ALLOW was refused: %v", err)
				}
				return
			}
			if reason, _, ok := Denial(err); !ok || reason != ReasonUnevaluableActor {
				t.Fatalf("err = %v, want PermissionDenied with %s", err, ReasonUnevaluableActor)
			}
		})
	}
}
