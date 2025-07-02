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
	"net/http"

	"github.com/llm-d/llm-d-routing-sidecar/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
)

var (
	// ChatCompletionsPath is the OpenAI chat completions path
	ChatCompletionsPath = "/v1/chat/completions"

	// CompletionsPath is the legacy completions path
	CompletionsPath = "/v1/completions"
)

func (s *Server) chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracing.StartHTTPSpan(r.Context(), tracing.OperationChatCompletions, r.Method, r.URL.Path)
	defer span.End()

	prefillPodURL := r.Header.Get(requestHeaderPrefillURL)
	requestID := r.Header.Get(requestHeaderRequestID)

	span.SetAttributes(
		attribute.String(tracing.AttrProxyConnector, s.connector),
		attribute.String(tracing.AttrProxyDecoderURL, s.decoderURL.String()),
	)

	if requestID != "" {
		span.SetAttributes(attribute.String(tracing.AttrProxyRequestID, requestID))
	}

	if prefillPodURL == "" {
		s.logger.V(4).Info("skip disagreggated prefill")
		span.SetAttributes(
			attribute.Bool("llm_d.proxy.disaggregated_prefill", false),
		)
		// Update the request context for downstream handlers
		*r = *r.WithContext(ctx)
		s.decoderProxy.ServeHTTP(w, r)
		return
	}

	span.SetAttributes(
		attribute.Bool("llm_d.proxy.disaggregated_prefill", true),
		attribute.String(tracing.AttrProxyPrefillerURL, prefillPodURL),
	)

	*r = *r.WithContext(ctx)
	s.runConnectorProtocol(w, r, prefillPodURL)
}
