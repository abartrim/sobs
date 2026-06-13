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

require (
	github.com/c-bata/go-prompt v0.2.6 // indirect
	github.com/chdb-io/chdb-go v1.11.0 // indirect
	github.com/ebitengine/purego v0.8.2 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.15 // indirect
	github.com/mattn/go-tty v0.0.5 // indirect
	github.com/pkg/term v1.2.0-beta.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	golang.org/x/sys v0.22.0 // indirect
)
