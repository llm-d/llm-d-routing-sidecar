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

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/llm-d/llm-d-routing-sidecar/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func (s *Server) runNIXLProtocolV2(w http.ResponseWriter, r *http.Request, prefillPodURL string) {
	ctx, span := tracing.StartProxySpan(r.Context(), tracing.OperationNIXLV2Protocol, ConnectorNIXLV2)
	defer span.End()

	start := time.Now()
	s.logger.V(4).Info("running NIXL protocol V2", "url", prefillPodURL)
	span.SetAttributes(
		attribute.String(tracing.AttrProxyPrefillerURL, prefillPodURL),
	)

	// Read request body
	defer r.Body.Close() //nolint:all
	original, err := io.ReadAll(r.Body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to read request body")
		w.WriteHeader(http.StatusBadRequest) // TODO: check FastAPI error code when failing to read body
		w.Write([]byte(err.Error()))         //nolint:all
		return
	}

	span.SetAttributes(attribute.Int(tracing.AttrHTTPRequestBodySize, len(original)))

	// Parse completion request
	var completionRequest map[string]any
	if err := json.Unmarshal(original, &completionRequest); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to parse JSON request")
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}

	// Extract model and other attributes from request for tracing
	if model, ok := completionRequest["model"].(string); ok {
		span.SetAttributes(attribute.String(tracing.AttrGenAIRequestModel, model))
	}
	if maxTokens, ok := completionRequest["max_tokens"].(float64); ok {
		span.SetAttributes(attribute.Int(tracing.AttrGenAIRequestMaxTokens, int(maxTokens)))
	}

	// Generate unique request UUID
	uuid, err := uuid.NewUUID()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to generate request UUID")
		if err := errorBadGateway(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}
	uuidStr := uuid.String()
	span.SetAttributes(attribute.String(tracing.AttrGenAIResponseID, uuidStr))

	// Prefill Stage

	// 1. Prepare prefill request
	preq := r.Clone(ctx)

	preq.Header.Add(requestHeaderRequestID, uuidStr)

	streamValue, streamOk := completionRequest[requestFieldStream]
	streamOptionsValue, streamOptionsOk := completionRequest[requestFieldStreamOptions]
	maxTokensValue, maxTokensOk := completionRequest[requestFieldMaxTokens]

	completionRequest[requestFieldKVTransferParams] = map[string]any{
		requestFieldDoRemoteDecode:  true,
		requestFieldDoRemotePrefill: false,
		requestFieldRemoteEngineID:  nil,
		requestFieldRemoteBlockIDs:  nil,
		requestFieldRemoteHost:      nil,
		requestFieldRemotePort:      nil,
	}

	completionRequest[requestFieldStream] = false
	delete(completionRequest, requestFieldStreamOptions)
	completionRequest[requestFieldMaxTokens] = 1

	pbody, err := json.Marshal(completionRequest)
	if err != nil {
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}
	preq.Body = io.NopCloser(strings.NewReader(string(pbody)))
	preq.ContentLength = int64(len(pbody))

	prefillHandler, err := s.prefillerProxyHandler(prefillPodURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to create prefiller proxy handler")
		if err := errorBadGateway(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}

	// 2. Forward request to prefiller
	prefillerStart := time.Now()
	_, prefillerSpan := tracing.StartSpan(ctx, tracing.OperationPrefillerForward, trace.SpanKindClient)
	prefillerSpan.SetAttributes(
		attribute.String(tracing.AttrProxyPrefillerURL, prefillPodURL),
	)

	s.logger.V(5).Info("sending request to prefiller", "url", prefillPodURL, "body", string(pbody))
	pw := &bufferedResponseWriter{}
	prefillHandler.ServeHTTP(pw, preq)

	prefillerSpan.SetAttributes(
		attribute.Int(tracing.AttrHTTPStatusCode, pw.statusCode),
		attribute.Float64("llm_d.proxy.prefiller_duration_ms", float64(time.Since(prefillerStart).Nanoseconds())/1e6),
	)

	if pw.statusCode < 200 || pw.statusCode >= 300 {
		prefillerSpan.SetStatus(codes.Error, "prefiller request failed")
		prefillerSpan.End()
		span.SetStatus(codes.Error, "prefiller request failed")
		s.logger.Error(err, "request failed", "code", pw.statusCode)
		w.WriteHeader(pw.statusCode)
		return
	}

	prefillerSpan.SetStatus(codes.Ok, "prefiller request completed")
	prefillerSpan.End()

	// Process response - extract p/d fields
	var prefillerResponse map[string]any
	if err := json.Unmarshal([]byte(pw.buffer.String()), &prefillerResponse); err != nil {
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}

	// 3. Verify response

	pKVTransferParams, ok := prefillerResponse[requestFieldKVTransferParams]
	if !ok {
		s.logger.Info("warning: missing 'kv_transfer_params' field in prefiller response")
	}

	s.logger.V(5).Info("received prefiller response", requestFieldKVTransferParams, pKVTransferParams)

	// Decode Stage

	// 1. Prepare decode request
	dreq := r.Clone(ctx)

	dreq.Header.Add(requestHeaderRequestID, uuidStr)

	delete(completionRequest, requestFieldStream)
	if streamOk {
		completionRequest[requestFieldStream] = streamValue
	}
	if streamOptionsOk {
		completionRequest[requestFieldStreamOptions] = streamOptionsValue
	}
	delete(completionRequest, requestFieldMaxTokens)
	if maxTokensOk {
		completionRequest[requestFieldMaxTokens] = maxTokensValue
	}
	completionRequest[requestFieldKVTransferParams] = pKVTransferParams

	dbody, err := json.Marshal(completionRequest)
	if err != nil {
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}
	dreq.Body = io.NopCloser(strings.NewReader(string(dbody)))
	dreq.ContentLength = int64(len(dbody))

	// 2. Forward to local decoder.
	decoderStart := time.Now()
	_, decoderSpan := tracing.StartSpan(ctx, tracing.OperationDecoderForward, trace.SpanKindClient)
	decoderSpan.SetAttributes(
		attribute.String(tracing.AttrProxyDecoderURL, s.decoderURL.String()),
	)

	s.logger.V(5).Info("sending request to decoder", "body", string(dbody))
	s.decoderProxy.ServeHTTP(w, dreq)

	decoderSpan.SetAttributes(
		attribute.Float64("llm_d.proxy.decoder_duration_ms", float64(time.Since(decoderStart).Nanoseconds())/1e6),
	)
	decoderSpan.End()

	// Record overall protocol timing
	span.SetAttributes(
		attribute.Float64(tracing.AttrProxyDecisionTime, float64(time.Since(start).Nanoseconds())/1e6),
	)
	span.SetStatus(codes.Ok, "NIXL V2 protocol completed successfully")
}
