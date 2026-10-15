module github.com/slaghuis/yc-platform/services/yc-audit

go 1.26.0

require (
	github.com/go-chi/chi/v5 v5.5.0
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/oapi-codegen/runtime v1.7.0
	github.com/slaghuis/yc-platform/libs/yc-authctx v0.0.0
	github.com/slaghuis/yc-platform/libs/yc-config v0.0.0
	github.com/slaghuis/yc-platform/libs/yc-dbkit v0.0.0
	github.com/slaghuis/yc-platform/libs/yc-errors v0.0.0
	github.com/slaghuis/yc-platform/libs/yc-eventbus v0.0.0
	github.com/slaghuis/yc-platform/libs/yc-events v0.0.0
	github.com/slaghuis/yc-platform/libs/yc-httpkit v0.0.0
	github.com/slaghuis/yc-platform/libs/yc-money v0.0.0
	github.com/slaghuis/yc-platform/libs/yc-telemetry v0.0.0
	github.com/stretchr/testify v1.12.1
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/golang-migrate/migrate/v4 v4.20.1 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.31.0 // indirect
	github.com/jackc/pgerrcode v0.0.0-20220416144525-469b46aa5efa // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/knadh/koanf/maps v0.1.2 // indirect
	github.com/knadh/koanf/parsers/yaml v1.1.1 // indirect
	github.com/knadh/koanf/providers/env v1.1.0 // indirect
	github.com/knadh/koanf/providers/rawbytes v1.0.1 // indirect
	github.com/knadh/koanf/v2 v2.3.8 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/nats-io/nats.go v1.54.0 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261012220534-4341edf3f941 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261012220534-4341edf3f941 // indirect
	google.golang.org/grpc v1.84.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace (
	github.com/slaghuis/yc-platform/libs/yc-authctx => ../../libs/yc-authctx
	github.com/slaghuis/yc-platform/libs/yc-config => ../../libs/yc-config
	github.com/slaghuis/yc-platform/libs/yc-dbkit => ../../libs/yc-dbkit
	github.com/slaghuis/yc-platform/libs/yc-errors => ../../libs/yc-errors
	github.com/slaghuis/yc-platform/libs/yc-eventbus => ../../libs/yc-eventbus
	github.com/slaghuis/yc-platform/libs/yc-events => ../../libs/yc-events
	github.com/slaghuis/yc-platform/libs/yc-httpkit => ../../libs/yc-httpkit
	github.com/slaghuis/yc-platform/libs/yc-money => ../../libs/yc-money
	github.com/slaghuis/yc-platform/libs/yc-telemetry => ../../libs/yc-telemetry
	github.com/slaghuis/yc-platform/libs/yc-testkit => ../../libs/yc-testkit
)
