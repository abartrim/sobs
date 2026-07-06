module github.com/stechly/sobs

go 1.26.2

require (
	github.com/chdb-io/chdb-go v1.11.0
	github.com/fernet/fernet-go v0.0.0-20240119011108-303da6aec611
	github.com/nikolalohinski/gonja/v2 v2.8.0
	go.opentelemetry.io/proto/otlp v1.10.0
	golang.org/x/crypto v0.53.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/ebitengine/purego v0.8.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260209200024-4cfbd4190f57 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260209200024-4cfbd4190f57 // indirect
	google.golang.org/grpc v1.79.2 // indirect
)

replace github.com/nikolalohinski/gonja/v2 => ./third_party/gonja
