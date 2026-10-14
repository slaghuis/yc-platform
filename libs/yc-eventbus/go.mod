module github.com/<org>/yc-platform/libs/yc-eventbus

go 1.22

require (
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.6.0
	github.com/nats-io/nats.go v1.37.0
	github.com/stretchr/testify v1.9.0
	github.com/<org>/yc-platform/libs/yc-telemetry v0.0.0
	go.opentelemetry.io/otel v1.28.0
	go.opentelemetry.io/otel/metric v1.28.0
	go.opentelemetry.io/otel/trace v1.28.0
)

replace github.com/<org>/yc-platform/libs/yc-telemetry => ../yc-telemetry
