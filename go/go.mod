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
// Until Phase 0 wires chdb-go, the base module builds with the standard library only.
// The Phase 0 gate test lives behind the `chdb` build tag so default builds stay clean.
