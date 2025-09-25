/*
Copyright 2025 The llm-d Authors.

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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var (
	// ChatCompletionsPath is the OpenAI chat completions path
	ChatCompletionsPath = "/v1/chat/completions"

	// CompletionsPath is the legacy completions path
	CompletionsPath = "/v1/completions"
)

func (s *Server) chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	tracer := otel.GetTracerProvider().Tracer("llm-d-routing-sidecar")
	ctx, span := tracer.Start(r.Context(), "routing_proxy.request")
	defer span.End()

	span.SetAttributes(
		attribute.String("llm_d.proxy.connector", s.config.Connector),
	)

	prefillPodHostPort := r.Header.Get(requestHeaderPrefillHostPort)
	if prefillPodHostPort == "" {
		// backward compatible behavior: to remove in next release
		prefillPodHostPort = r.Header.Get(requestHeaderPrefillURL)
	}

	if prefillPodHostPort == "" {
		s.logger.V(4).Info("skip disaggregated prefill")
		// Update the request context for downstream handlers
		r = r.WithContext(ctx)
		s.decoderProxy.ServeHTTP(w, r)
		return
	}

	// SSRF Protection: Check if the prefill target is allowed
	if !s.allowlistValidator.IsAllowed(prefillPodHostPort) {
		s.logger.Error(nil, "SSRF protection: prefill target not in allowlist",
			"target", prefillPodHostPort,
			"clientIP", r.RemoteAddr,
			"userAgent", r.Header.Get("User-Agent"),
			"requestPath", r.URL.Path)
		http.Error(w, "Forbidden: prefill target not allowed by SSRF protection", http.StatusForbidden)
		return
	}

	s.logger.V(4).Info("SSRF protection: prefill target allowed", "target", prefillPodHostPort)

	r = r.WithContext(ctx)
	s.runConnectorProtocol(w, r, prefillPodHostPort)
}
