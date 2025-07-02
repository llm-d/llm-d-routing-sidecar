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

	// Span operation names
	OperationProxyRequest        = "routing_proxy.request"
	OperationChatCompletions     = "routing_proxy.chat_completions"
	OperationNIXLV2Protocol      = "routing_proxy.nixlv2_protocol"
	OperationPrefillerForward    = "routing_proxy.prefiller_forward"
	OperationDecoderForward      = "routing_proxy.decoder_forward"

	// Semantic conventions
	AttrProxyConnector         = "llm_d.proxy.connector"
	AttrProxyDecoderURL        = "llm_d.proxy.decoder_url"
	AttrProxyPrefillerURL      = "llm_d.proxy.prefiller_url"
	AttrProxyRequestID         = "llm_d.proxy.request_id"
	AttrProxyDecisionTime      = "llm_d.routing.decision_time"

	// GenAI semantic conventions
	AttrGenAIRequestModel      = "gen_ai.request.model"
	AttrGenAIRequestMaxTokens  = "gen_ai.request.max_tokens"
	AttrGenAIResponseID        = "gen_ai.response.id"

	AttrHTTPMethod             = "http.request.method"
	AttrHTTPRoute              = "http.route"
	AttrHTTPStatusCode         = "http.response.status_code"
	AttrHTTPRequestBodySize    = "http.request.body.size"
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
		// Set up a no-op tracer provider but maintain context propagation
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
		return nil, err
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(config.ExporterEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
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
func StartSpan(ctx context.Context, operationName string, spanKind trace.SpanKind) (context.Context, trace.Span) {
	tracer := otel.Tracer(ServiceName)
	ctx, span := tracer.Start(ctx, operationName,
		trace.WithSpanKind(spanKind),
	)
	return ctx, span
}

// StartHTTPSpan creates a new span for HTTP operations
func StartHTTPSpan(ctx context.Context, operationName, method, route string) (context.Context, trace.Span) {
	ctx, span := StartSpan(ctx, operationName, trace.SpanKindServer)
	span.SetAttributes(
		attribute.String(AttrHTTPMethod, method),
		attribute.String(AttrHTTPRoute, route),
	)
	return ctx, span
}

// StartProxySpan creates a new span for proxy forwarding operations
func StartProxySpan(ctx context.Context, operationName, connector string) (context.Context, trace.Span) {
	ctx, span := StartSpan(ctx, operationName, trace.SpanKindClient)
	span.SetAttributes(
		attribute.String(AttrProxyConnector, connector),
	)
	return ctx, span
}
