package telemetry

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

func Configure(ctx context.Context, endpoint, version, environment string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exporterOptions := []otlptracegrpc.Option{otlptracegrpc.WithEndpointURL(endpoint)}
	if strings.HasPrefix(endpoint, "http://") {
		exporterOptions = append(exporterOptions, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, exporterOptions...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName("ocserv-control"), semconv.ServiceVersion(version), semconv.DeploymentEnvironmentName(environment),
	))
	if err != nil {
		return nil, fmt.Errorf("create telemetry resource: %w", err)
	}
	provider := tracesdk.NewTracerProvider(tracesdk.WithBatcher(exporter), tracesdk.WithResource(res))
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}
