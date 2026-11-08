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
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	sensiveclient "github.com/sensive-ai/sensive-go/client"
	accessv1 "github.com/sensive-ai/sensive-go/client/gen/sensive/access/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

// The gRPC conformance corpus, which the module ships in
// testdata: every case is run through this runtime's reading of the
// annotations, so the Go reading and any other language's are one reading.
const (
	fixturesPath = "testdata/conformance/fixtures.binpb"
	casesPath    = "testdata/conformance/cases.yaml"
)

type conformanceCase struct {
	Name   string            `yaml:"name"`
	Method string            `yaml:"method"`
	Actor  string            `yaml:"actor"`
	Owner  map[string]string `yaml:"owner"`
	Expect struct {
		Request    any    `yaml:"request"`
		Error      string `yaml:"error"`
		Ungated    bool   `yaml:"ungated"`
		OwnerField string `yaml:"owner_field"`
	} `yaml:"expect"`
}

func loadFixtures(t *testing.T) *protoregistry.Files {
	t.Helper()
	raw, err := os.ReadFile(fixturesPath)
	if err != nil {
		t.Fatalf("reading the fixtures: %v (has the generate script copied the corpus?)", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatalf("decoding the fixtures: %v", err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		t.Fatalf("building the fixture descriptors: %v", err)
	}
	return files
}

func loadCases(t *testing.T) []conformanceCase {
	t.Helper()
	raw, err := os.ReadFile(casesPath)
	if err != nil {
		t.Fatalf("reading the cases: %v", err)
	}
	var cases []conformanceCase
	if err := yaml.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decoding the cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	return cases
}

// asJSON normalizes a value through JSON, so a request the runtime built and
// an expectation YAML parsed compare as the wire form. A message is written
// as protobuf's JSON mapping writes it, which is the checker's JSON.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	var raw []byte
	var err error
	if m, ok := v.(proto.Message); ok {
		raw, err = protojson.Marshal(m)
	} else {
		raw, err = json.Marshal(v)
	}
	if err != nil {
		t.Fatalf("marshalling %T: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	return out
}

func TestConformance(t *testing.T) {
	files := loadFixtures(t)
	for _, tc := range loadCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			desc, err := files.FindDescriptorByName(protoreflect.FullName(tc.Method))
			if err != nil {
				t.Fatalf("fixture method %s: %v", tc.Method, err)
			}
			method := desc.(protoreflect.MethodDescriptor)

			ann, err := annotationOf(method)
			switch {
			case tc.Expect.Ungated:
				if err != nil || ann != nil {
					t.Fatalf("want an unannotated method; got annotation %+v, err %v", ann, err)
				}
			case tc.Expect.Error != "":
				if err == nil {
					t.Fatalf("want an error containing %q; got annotation %+v", tc.Expect.Error, ann)
				}
				if !strings.Contains(err.Error(), tc.Expect.Error) {
					t.Fatalf("error %q does not mention %q", err, tc.Expect.Error)
				}
			default:
				if err != nil {
					t.Fatalf("annotation: %v", err)
				}
				gotField := ""
				if ann.OwnerField != nil {
					gotField = string(ann.OwnerField.Name())
				}
				if gotField != tc.Expect.OwnerField {
					t.Fatalf("owner field = %q, want %q", gotField, tc.Expect.OwnerField)
				}
				var owner *accessv1.Owner
				if id, ok := tc.Owner["id"]; ok {
					owner = sensiveclient.OwnerID(id)
				} else if email, ok := tc.Owner["email"]; ok {
					owner = sensiveclient.OwnerEmail(email)
				}
				got := asJSON(t, ann.Request(tc.Actor, owner))
				want := asJSON(t, tc.Expect.Request)
				if !reflect.DeepEqual(got, want) {
					g, _ := json.MarshalIndent(got, "", "  ")
					w, _ := json.MarshalIndent(want, "", "  ")
					t.Fatalf("request differs from the corpus\n got: %s\nwant: %s", g, w)
				}
			}
		})
	}
}

func TestFullNameOf(t *testing.T) {
	if got := fullNameOf("/conformance.v1.Ledger/ListTransactions"); got != "conformance.v1.Ledger.ListTransactions" {
		t.Errorf("fullNameOf = %q", got)
	}
}
