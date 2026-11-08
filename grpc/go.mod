module github.com/sensive-ai/sensive-go/grpc

go 1.26.0

// The client is the sibling module in this repository. Go ignores a replace
// directive outside the module being built, so a customer's build takes the
// client at the version required below.
replace github.com/sensive-ai/sensive-go/client => ../client

require (
	github.com/sensive-ai/sensive-go/client v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.60.0
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require (
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)
