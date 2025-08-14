/*
Copyright 2025 IBM.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tracing

import (
	"context"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.25.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	ServiceName = "llm-d-routing-sidecar"

	envOTELTracingEnabled     = "OTEL_TRACING_ENABLED"
	envOTELExporterEndpoint   = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envOTELServiceName        = "OTEL_SERVICE_NAME"
	envOTELSamplingRate       = "OTEL_SAMPLING_RATE"
)

type Config struct {
	Enabled          bool
	ExporterEndpoint string
	SamplingRate     float64
	ServiceName      string
}

func NewConfigFromEnv() *Config {
	config := &Config{
		Enabled:          false,
		ExporterEndpoint: "http://localhost:4317",
		SamplingRate:     0.1,
		ServiceName:      ServiceName,
	}

	if enabled := os.Getenv(envOTELTracingEnabled); enabled != "" {
		if enabledBool, err := strconv.ParseBool(enabled); err == nil {
			config.Enabled = enabledBool
		}
	}

	if endpoint := os.Getenv(envOTELExporterEndpoint); endpoint != "" {
		config.ExporterEndpoint = endpoint
	}

	if serviceName := os.Getenv(envOTELServiceName); serviceName != "" {
		config.ServiceName = serviceName
	}

	if samplingRate := os.Getenv(envOTELSamplingRate); samplingRate != "" {
		if rate, err := strconv.ParseFloat(samplingRate, 64); err == nil && rate >= 0.0 && rate <= 1.0 {
			config.SamplingRate = rate
		}
	}

	return config
}

func Initialize(ctx context.Context, config *Config) (func(), error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if !config.Enabled {
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return func() {}, nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(config.ServiceName),
			semconv.ServiceVersionKey.String("dev"),
			attribute.String("service.component", "routing-proxy"),
		),
	)
	if err != nil {
		// Fall back to no-op tracer if resource creation fails
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return func() {}, nil
	}

	// Create a timeout context for exporter creation to avoid blocking startup
	exporterCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	exporter, err := otlptracegrpc.New(exporterCtx,
		otlptracegrpc.WithEndpoint(config.ExporterEndpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(2*time.Second),
	)
	if err != nil {
		// Fall back to no-op tracer if collector is unreachable
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return func() {}, nil
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(time.Second*5),
			sdktrace.WithMaxExportBatchSize(512),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(config.SamplingRate)),
	)

	otel.SetTracerProvider(tp)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		tp.Shutdown(ctx)
	}, nil
}

// StartSpan creates a new span for routing proxy operations
func StartSpan(ctx context.Context, operationName string) (context.Context, trace.Span) {
	tracer := otel.Tracer(ServiceName)
	return tracer.Start(ctx, operationName)
}
