package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

const instrumentation = "samuori.invalid/samuori"

// TracingConfig enables OpenTelemetry tracing over OTLP/HTTP. Endpoint is the full traces URL, such as
// http://collector:4318/v1/traces; empty disables tracing.
type TracingConfig struct {
	Endpoint    string
	SampleRatio float64
	Service     string
	Version     string
}

// SetupTracing installs the global tracer provider and W3C trace-context propagation. The returned function
// flushes and stops the exporter.
func SetupTracing(ctx context.Context, c TracingConfig) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if c.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("tracing endpoint %q: must be an http or https URL", c.Endpoint)
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(c.Endpoint))
	if err != nil {
		return nil, err
	}
	return Install(exp, c), nil
}

// Install uses exporter for the global tracer provider; tests pass an in-memory exporter.
func Install(exporter sdktrace.SpanExporter, c TracingConfig) func(context.Context) error {
	ratio := c.SampleRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 1
	}
	service := c.Service
	if service == "" {
		service = "samuori-server"
	}
	res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(service), semconv.ServiceVersion(c.Version))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown
}

// Span is a started span. Domain packages use it instead of the OpenTelemetry API.
type Span struct{ s trace.Span }

// Start begins a span named name. Attributes must never carry secret values or credentials.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, Span) {
	ctx, s := otel.Tracer(instrumentation).Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, Span{s}
}

// String is a span attribute.
func String(k, v string) attribute.KeyValue { return attribute.String(k, v) }

// Int is a span attribute.
func Int(k string, v int) attribute.KeyValue { return attribute.Int(k, v) }

func (s Span) Set(attrs ...attribute.KeyValue) { s.s.SetAttributes(attrs...) }

// End finishes the span, marking it failed when err is not nil. The error text is recorded, so callers redact
// credentials first, as they already do for stored errors.
func (s Span) End(err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		s.s.SetStatus(codes.Error, err.Error())
	}
	s.s.End()
}

// TraceID returns the current trace ID, or "" outside a sampled trace.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// TraceParent returns the W3C traceparent of the current span, or "" outside a sampled trace.
func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// WithTraceParent continues the trace a W3C traceparent names; malformed values are ignored.
func WithTraceParent(ctx context.Context, traceParent string) context.Context {
	if traceParent == "" || len(traceParent) > 64 {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": traceParent})
}
