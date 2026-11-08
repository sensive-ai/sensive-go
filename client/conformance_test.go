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
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The environment contract every Sensive SDK keeps, as cases, which the
// module ships in testdata.
type corpus struct {
	Environment []struct {
		Name     string            `json:"name"`
		Env      map[string]string `json:"env"`
		Settings *struct {
			Mode        string `json:"mode"`
			DACURL      string `json:"dac_url"`
			Environment string `json:"environment"`
			TimeoutMS   int64  `json:"timeout_ms"`
		} `json:"settings"`
		Refuses string `json:"refuses"`
	} `json:"environment"`
	Checks []struct {
		Name    string            `json:"name"`
		Env     map[string]string `json:"env"`
		Checker string            `json:"checker"`
		Waits   bool              `json:"waits"`
		Sent    *struct {
			Mode        string `json:"mode"`
			Environment string `json:"environment"`
		} `json:"sent"`
		Result struct {
			Decision string `json:"decision"`
			Token    bool   `json:"token"`
			Source   string `json:"source"`
		} `json:"result"`
	} `json:"checks"`
	Failures []struct {
		Name   string            `json:"name"`
		Env    map[string]string `json:"env"`
		Result struct {
			Decision string `json:"decision"`
			Token    bool   `json:"token"`
			Source   string `json:"source"`
		} `json:"result"`
	} `json:"failures"`
}

func readCorpus(t *testing.T) corpus {
	t.Helper()
	data, err := os.ReadFile("testdata/environment.json")
	if err != nil {
		t.Fatalf("the corpus (run the generate script): %v", err)
	}
	var c corpus
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnvironmentConformance(t *testing.T) {
	for _, tc := range readCorpus(t).Environment {
		t.Run(tc.Name, func(t *testing.T) {
			s, err := settingsFrom(lookupIn(tc.Env))
			if tc.Refuses != "" {
				refused, ok := err.(*ConfigError)
				if !ok || refused.Variable != tc.Refuses {
					t.Fatalf("err = %v, want a refusal of %s", err, tc.Refuses)
				}
				return
			}
			if err != nil {
				t.Fatalf("settings: %v", err)
			}
			want := tc.Settings
			if string(s.mode) != want.Mode {
				t.Errorf("mode = %s, want %s", s.mode, want.Mode)
			}
			if s.mode == ModeDisabled {
				return
			}
			if s.dacURL != want.DACURL ||
				s.environment.String() != want.Environment || s.timeout.Milliseconds() != want.TimeoutMS {
				t.Errorf("settings = %+v, want %+v", s, *want)
			}
		})
	}
}

// fake answers a check as a case's checker does.
func fake(behavior string) func(ctx context.Context) (*accessv1.AccessCheckResponse, error) {
	decision := func(d accessv1.AccessDecision_Value) func(context.Context) (*accessv1.AccessCheckResponse, error) {
		return answering(&accessv1.AccessCheckResponse{Result: d.Enum(), DecisionToken: proto.String("decision-token")}, nil)
	}
	switch behavior {
	case "ALLOW":
		return decision(accessv1.AccessDecision_ALLOW)
	case "DENY":
		return decision(accessv1.AccessDecision_DENY)
	case "NO_DECISION":
		return answering(&accessv1.AccessCheckResponse{}, nil)
	case "ERROR":
		return answering(nil, status.Error(codes.Unavailable, "unavailable"))
	case "HANG":
		return func(ctx context.Context) (*accessv1.AccessCheckResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	panic("unknown checker behavior " + behavior)
}

func TestCheckConformance(t *testing.T) {
	for _, tc := range readCorpus(t).Checks {
		t.Run(tc.Name, func(t *testing.T) {
			c := newChecker(fake(tc.Checker))
			// A check the client must not wait for is held unanswered until
			// it has returned.
			c.hold = !tc.Waits
			checkerURL := serve(t, c, nil)
			env := map[string]string{}
			for name, value := range tc.Env {
				env[name] = strings.ReplaceAll(value, "$CHECKER", checkerURL)
			}
			client := clientFor(t, env)

			result := client.CheckAccess(context.Background(), sampleRequest())
			close(c.release)

			if result.Decision.String() != tc.Result.Decision || string(result.Source) != tc.Result.Source ||
				(result.DecisionToken != "") != tc.Result.Token {
				t.Errorf("result = %+v, want %+v", result, tc.Result)
			}
			if tc.Sent == nil {
				select {
				case got := <-c.arrived:
					t.Errorf("a check was sent: %v", got.request)
				default:
				}
				return
			}
			got := <-c.arrived
			if got.request.GetMode().String() != tc.Sent.Mode ||
				got.request.GetUsage().GetEnvironment().String() != tc.Sent.Environment {
				t.Errorf("sent mode %s, environment %s; want mode %s, environment %s",
					got.request.GetMode(), got.request.GetUsage().GetEnvironment(), tc.Sent.Mode, tc.Sent.Environment)
			}
		})
	}
}

func TestFailureConformance(t *testing.T) {
	for _, tc := range readCorpus(t).Failures {
		t.Run(tc.Name, func(t *testing.T) {
			result := clientFor(t, tc.Env).Failed(errors.New("the owner lookup failed"))
			if result.Decision.String() != tc.Result.Decision || string(result.Source) != tc.Result.Source ||
				(result.DecisionToken != "") != tc.Result.Token {
				t.Errorf("result = %+v, want %+v", result, tc.Result)
			}
		})
	}
}
