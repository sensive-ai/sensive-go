module github.com/sensive-ai/sensive-go/grpc

go 1.26.0

// The client is the sibling module in this repository. Go ignores a replace
// directive outside the module being built, so a customer's build takes the
// client at the version required below.
replace github.com/sensive-ai/sensive-go/client => ../client

require (
	github.com/sensive-ai/sensive-go/client v0.1.1
	golang.org/x/net v0.58.0
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217
	google.golang.org/grpc v1.77.0
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)
