package dbkit

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const maxStatementAttr = 512

// queryTracer creates a client span per query. Statements are
// parameterised, so the SQL text carries no request values.
type queryTracer struct{}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// Only trace queries that belong to a request or job; the relay's idle
	// polling would otherwise flood the trace backend with root spans.
	if !trace.SpanFromContext(ctx).SpanContext().IsValid() {
		return ctx
	}
	sql := strings.Join(strings.Fields(data.SQL), " ")
	ctx, _ = otel.Tracer("yc-dbkit").Start(ctx, "db."+operation(sql),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.statement", truncate(sql, maxStatementAttr)),
		),
	)
	return ctx
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, data.Err.Error())
	}
	span.End()
}

func operation(sql string) string {
	op, _, _ := strings.Cut(sql, " ")
	switch op = strings.ToLower(op); op {
	case "select", "insert", "update", "delete", "with", "begin", "commit", "rollback":
		return op
	default:
		return "query"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
