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

	// 1. Parse request body
	requestData, err := s.parseSGLangRequest(r)
	if err != nil {
		s.logger.Error(err, "Failed to parse request")
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	// 2. Validate prefill host
	if prefillPodHostPort == "" {
		http.Error(w, "Prefill host required for SGLang P/D disaggregation", http.StatusBadRequest)
		return
	}

	// 3. Get decode host from header or use default
	decodeHost := r.Header.Get("x-decoder-host-port")
	if decodeHost == "" {
		decodeHost = strings.TrimPrefix(s.decoderURL.String(), "http://")
		decodeHost = strings.TrimPrefix(decodeHost, "https://")
	}

	// 4. Send concurrent prefill and decode requests
	s.sendSGLangConcurrentRequests(r.Context(), w, r, prefillPodHostPort, decodeHost, requestData)
}

func (s *Server) sendSGLangConcurrentRequests(ctx context.Context, w http.ResponseWriter, r *http.Request, prefillHost, decodeHost string, requestData map[string]interface{}) {
	// 1. Generate a unique room ID for this request
	roomID := s.generateSGLangRoomID()

	// 2. Inject bootstrap info for both prefill and decode
	prefillRequest := s.addSGLangBootstrapInfo(requestData, prefillHost, roomID)
	decodeRequest := s.addSGLangBootstrapInfo(requestData, prefillHost, roomID)

	// 3. Send prefill request asynchronously (fire-and-forget)
	go func() {
		s.logger.V(5).Info("sending prefill request", "room_id", roomID)
		resp, err := s.sendSGLangPrefillRequest(ctx, r, prefillHost, prefillRequest)
		if err != nil {
			s.logger.Error(err, "prefill request failed")
			return
		}
		// Close response body to free resources
		if resp != nil {
			resp.Body.Close()
		}
		s.logger.V(5).Info("prefill request completed", "status", resp.StatusCode, "room_id", roomID)
	}()

	// 4. Send decode request synchronously and wait for response
	s.logger.V(5).Info("sending decode request", "room_id", roomID)
	decodeResp, err := s.sendSGLangDecodeRequest(ctx, r, decodeRequest, decodeHost)
	if err != nil {
		s.logger.Error(err, "decode request failed")
		http.Error(w, "Decode request failed", http.StatusInternalServerError)
		return
	}

	// 5. Return decode response
	s.handleSGLangDecodeResponse(w, decodeResp)
}

func (s *Server) addSGLangBootstrapInfo(requestData map[string]interface{}, prefillHost string, roomID int64) map[string]interface{} {
	modifiedRequest := make(map[string]interface{})
	for k, v := range requestData {
		modifiedRequest[k] = v
	}

	// 1. Generate bootstrap host from prefill host
	bootstrapHost := s.generateBootstrapHost(prefillHost)

	// 2. Get bootstrap port from environment variable
	bootstrapPort := 8998 // Default SGLang bootstrap port
	if portStr := os.Getenv("SGLANG_BOOTSTRAP_PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			bootstrapPort = port
		}
	}

	// 3. Add bootstrap information
	modifiedRequest["bootstrap_host"] = bootstrapHost
	modifiedRequest["bootstrap_port"] = bootstrapPort
	modifiedRequest["bootstrap_room"] = roomID
	modifiedRequest["bootstrap_room_id"] = roomID
	modifiedRequest["room_id"] = roomID

	// 4. Add string versions for SGLang compatibility
	if _, hasText := modifiedRequest["text"]; hasText {
		modifiedRequest["bootstrap_room_id_str"] = fmt.Sprintf("%d", roomID)
		modifiedRequest["room_id_str"] = fmt.Sprintf("%d", roomID)
	}

	s.logger.V(5).Info("bootstrap info added",
		"bootstrap_host", bootstrapHost,
		"bootstrap_port", bootstrapPort,
		"bootstrap_room", roomID)

	return modifiedRequest
}

func (s *Server) sendSGLangPrefillRequest(ctx context.Context, r *http.Request, prefillHost string, requestData map[string]interface{}) (*http.Response, error) {
	// 1. Marshal request data
	body, err := json.Marshal(requestData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal prefill request: %w", err)
	}

	// 2. Determine the correct endpoint for prefill
	endpoint := "/v1/chat/completions"
	if _, hasText := requestData["text"]; hasText {
		endpoint = "/generate"
	}

	// 3. Create HTTP request
	url := fmt.Sprintf("http://%s%s", prefillHost, endpoint)
	req, err := http.NewRequestWithContext(ctx, "POST", url, io.NopCloser(strings.NewReader(string(body))))
	if err != nil {
		return nil, fmt.Errorf("failed to create prefill request: %w", err)
	}

	// 4. Copy headers from original request
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")

	// 5. Send request
	client := &http.Client{Timeout: 30 * time.Second}
	return client.Do(req)
}

func (s *Server) sendSGLangDecodeRequest(ctx context.Context, r *http.Request, requestData map[string]interface{}, decodeHost string) (*http.Response, error) {
	// 1. Marshal request data
	body, err := json.Marshal(requestData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal decode request: %w", err)
	}

	// 2. Determine the correct endpoint for decode
	endpoint := "/v1/chat/completions"
	if _, hasText := requestData["text"]; hasText {
		endpoint = "/generate"
	}

	// 3. Create HTTP request
	url := fmt.Sprintf("http://%s%s", decodeHost, endpoint)
	req, err := http.NewRequestWithContext(ctx, "POST", url, io.NopCloser(strings.NewReader(string(body))))
	if err != nil {
		return nil, fmt.Errorf("failed to create decode request: %w", err)
	}

	// 4. Copy headers from original request
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")

	// 5. Send request
	client := &http.Client{Timeout: 30 * time.Second}
	return client.Do(req)
}

func (s *Server) handleSGLangDecodeResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	// 1. Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// 2. Set status code
	w.WriteHeader(resp.StatusCode)

	// 3. Copy response body
	_, err := io.Copy(w, resp.Body)
	if err != nil {
		s.logger.Error(err, "failed to write decode response")
		return
	}
}

func (s *Server) parseSGLangRequest(r *http.Request) (map[string]interface{}, error) {
	// 1. Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	// 2. Parse JSON data
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
	// 1. Extract hostname from prefill host (remove port)
	parts := strings.Split(prefillHost, ":")
	hostname := parts[0]

	return hostname
}
