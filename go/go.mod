module github.com/sobs/sobs

go 1.23

// Dependencies are added deliberately and justified in DEPENDENCIES.md. The target end
// state is exactly:
//
//   require (
//       github.com/chdb-io/chdb-go v0.0.0  // pinned in CHDB_PIN.md (build from main)
//       google.golang.org/protobuf vX.Y.Z  // OTLP ingest
//   )
//
// google.golang.org/protobuf is the protobuf runtime for OTLP-protobuf ingest. The OTLP
// message .pb.go types are vendored under internal/otlp/genpb (provenance in that dir's
// README) so the only new module dependency is the runtime itself — no grpc, no otlp module.

require (
	github.com/chdb-io/chdb-go v1.12.0
	github.com/dlclark/regexp2 v1.12.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/ebitengine/purego v0.8.2 // indirect
	golang.org/x/sys v0.22.0 // indirect
)
