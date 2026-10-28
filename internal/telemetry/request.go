package telemetry

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// RequestInfo is filled in while a request is handled, so the outer middleware can label metrics, spans and the
// request log with the API operation.
type RequestInfo struct {
	Operation string
}

type requestKey struct{}

// SetOperation names the API operation serving the request.
func SetOperation(ctx context.Context, op string) {
	if info, ok := ctx.Value(requestKey{}).(*RequestInfo); ok {
		info.Operation = op
	}
	trace.SpanFromContext(ctx).SetName(op)
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Instrument wraps an HTTP handler with a server span, request metrics and a completion callback for logging. The
// span records the method, operation and status; never the query string, headers or body.
func Instrument(next http.Handler, m *Metrics, done func(r *http.Request, info RequestInfo, status int, d time.Duration, traceID string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := otel.Tracer(instrumentation).Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)))
		info := &RequestInfo{}
		ctx = context.WithValue(ctx, requestKey{}, info)
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))
		d := time.Since(start)
		span.SetAttributes(attribute.String("samuori.operation", info.Operation), attribute.Int("http.response.status_code", rec.status))
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
		span.End()
		if info.Operation != "" {
			m.ObserveRequest(info.Operation, r.Method, rec.status, d)
		}
		if done != nil {
			done(r, *info, rec.status, d, TraceID(ctx))
		}
	})
}
