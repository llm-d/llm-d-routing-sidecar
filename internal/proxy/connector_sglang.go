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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func (s *Server) runSGLangProtocol(w http.ResponseWriter, r *http.Request, prefillPodHostPort string) {
	s.logger.V(4).Info("running SGLang protocol", "url", prefillPodHostPort)

	// Parse request body
	requestData, err := s.parseSGLangRequest(r)
	if err != nil {
		s.logger.Error(err, "failed to parse request")
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	// Validate prefill host
	if prefillPodHostPort == "" {
		http.Error(w, "Prefill host required for SGLang P/D disaggregation", http.StatusBadRequest)
		return
	}

	// Send concurrent prefill and decode requests
	s.sendSGLangConcurrentRequests(r.Context(), w, r, prefillPodHostPort, requestData)
}

func (s *Server) sendSGLangConcurrentRequests(ctx context.Context, w http.ResponseWriter, r *http.Request, prefillHost string, requestData map[string]interface{}) {
	// Generate a unique bootstrap room ID for this request
	roomID := s.generateSGLangRoomID()

	// Inject bootstrap info for both prefill and decode
	prefillRequest := s.addSGLangBootstrapInfo(requestData, prefillHost, roomID)
	decodeRequest := s.addSGLangBootstrapInfo(requestData, prefillHost, roomID)

	// Send prefill request asynchronously
	go func() {
		s.logger.V(5).Info("sending prefill request", "room_id", roomID, "prefill_host", prefillHost)

		prefillHandler, err := s.prefillerProxyHandler(prefillHost)
		if err != nil {
			s.logger.Error(err, "failed to get prefiller proxy handler", "prefill_host", prefillHost)
			return
		}
		s.logger.V(5).Info("got prefiller proxy handler", "prefill_host", prefillHost)

		prefillReq := r.Clone(ctx)

		body, err := json.Marshal(prefillRequest)
		if err != nil {
			s.logger.Error(err, "failed to marshal prefill request", "prefill_host", prefillHost)
			return
		}

		prefillReq.Body = io.NopCloser(strings.NewReader(string(body)))
		prefillReq.ContentLength = int64(len(body))

		s.logger.V(5).Info("created prefill request", "url", prefillReq.URL.String())

		// Use prefiller proxy handler (fire-and-forget)
		pw := &bufferedResponseWriter{}
		s.logger.V(5).Info("calling prefiller proxy handler", "url", prefillReq.URL.String())
		prefillHandler.ServeHTTP(pw, prefillReq)
		s.logger.V(5).Info("prefill request completed", "room_id", roomID, "status", pw.statusCode)
	}()

	// Send decode request synchronously
	s.logger.V(5).Info("sending decode request", "room_id", roomID)

	decodeReq := r.Clone(ctx)

	body, err := json.Marshal(decodeRequest)
	if err != nil {
		s.logger.Error(err, "failed to marshal decode request")
		http.Error(w, "Failed to marshal decode request", http.StatusInternalServerError)
		return
	}

	decodeReq.Body = io.NopCloser(strings.NewReader(string(body)))
	decodeReq.ContentLength = int64(len(body))

	s.logger.V(5).Info("calling decoder proxy", "url", decodeReq.URL.String())
	s.decoderProxy.ServeHTTP(w, decodeReq)
}

func (s *Server) addSGLangBootstrapInfo(requestData map[string]interface{}, prefillHost string, roomID int64) map[string]interface{} {
	modifiedRequest := make(map[string]interface{})
	for k, v := range requestData {
		modifiedRequest[k] = v
	}

	// Generate bootstrap host from prefill host
	bootstrapHost := s.generateBootstrapHost(prefillHost)

	// Get bootstrap port from environment variable
	bootstrapPort := 8998 // Default SGLang bootstrap port
	if portStr := os.Getenv("SGLANG_BOOTSTRAP_PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			bootstrapPort = port
		}
	}

	// Add bootstrap information
	modifiedRequest[requestFieldBootstrapHost] = bootstrapHost
	modifiedRequest[requestFieldBootstrapPort] = bootstrapPort
	modifiedRequest[requestFieldBootstrapRoom] = roomID
	modifiedRequest[requestFieldBootstrapRoomID] = roomID
	modifiedRequest[requestFieldRoomID] = roomID

	s.logger.V(5).Info("bootstrap info added",
		"bootstrap_host", bootstrapHost,
		"bootstrap_port", bootstrapPort,
		"bootstrap_room", roomID)

	return modifiedRequest
}

func (s *Server) parseSGLangRequest(r *http.Request) (map[string]interface{}, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	var requestData map[string]interface{}
	if err := json.Unmarshal(body, &requestData); err != nil {
		return nil, fmt.Errorf("failed to parse request body: %w", err)
	}

	return requestData, nil
}

func (s *Server) generateSGLangRoomID() int64 {
	return time.Now().UnixNano() + int64(rand.Intn(1000))
}

func (s *Server) generateBootstrapHost(prefillHost string) string {
	// Extract hostname from prefill host
	parts := strings.Split(prefillHost, ":")
	hostname := parts[0]

	return hostname
}
