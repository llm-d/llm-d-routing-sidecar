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

	"github.com/google/uuid"
)

var (
	ChatCompletionsPath = "/v1/chat/completions"
	CompletionsPath     = "/v1/completions"
)

func (s *Server) ChatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	prefillPodURL := r.Header.Get(RequestHeaderPrefillURL)

	if prefillPodURL == "" {
		s.logger.V(5).Info("skip disagreggated prefill")
		s.decoderProxy.ServeHTTP(w, r)
		return
	}

	s.runConnectorProtocol(w, r, prefillPodURL)
}

func (s *Server) runLMCacheProtocol(w http.ResponseWriter, r *http.Request, prefillPodURL string) {
	s.logger.Info("running LMCache protocol")

	// Read and parse request body
	defer r.Body.Close() //nolint:all
	original, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest) // TODO: check FastAPI error code when failing to read body
		w.Write([]byte(err.Error()))         //nolint:all
		return
	}

	// Parse completion request
	var completionRequest map[string]any
	if err := json.Unmarshal(original, &completionRequest); err != nil {
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}

	// Create prefiller request. Set max_tokens to 1.

	ctx := r.Context()
	preq := r.Clone(ctx)

	completionRequest[RequestFieldMaxTokens] = 1
	completionRequest[RequestFieldMaxCompletionTokens] = 1

	pbody, err := json.Marshal(completionRequest)
	if err != nil {
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}
	preq.Body = io.NopCloser(strings.NewReader(string(pbody)))
	preq.ContentLength = int64(len(pbody))

	// Forward request to prefiller

	prefillHandler, err := s.prefillerProxyHandler(prefillPodURL)
	if err != nil {
		if err := errorBadGateway(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}

	pw := &bufferedResponseWriter{}
	prefillHandler.ServeHTTP(pw, preq)

	if pw.statusCode < 200 || pw.statusCode >= 300 {
		s.logger.Error(err, "request failed", "code", pw.statusCode)
		w.WriteHeader(pw.statusCode)
		return
	}

	// Forward original request to local decoder

	r.Body = io.NopCloser(strings.NewReader(string(original)))
	s.decoderProxy.ServeHTTP(w, r)
}

func (s *Server) runNIXLProtocol(w http.ResponseWriter, r *http.Request, prefillPodURL string) {
	s.logger.Info("running NIXL protocol")

	// Read request body
	defer r.Body.Close() //nolint:all
	original, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest) // TODO: check FastAPI error code when failing to read body
		w.Write([]byte(err.Error()))         //nolint:all
		return
	}

	// Parse completion request
	var completionRequest map[string]any
	if err := json.Unmarshal(original, &completionRequest); err != nil {
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}

	// Generate unique request UUID
	uuid, err := uuid.NewUUID()
	if err != nil {
		if err := errorBadGateway(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}
	uuidStr := uuid.String()

	// Send request to prefill pod

	// 1. Prepare request
	ctx := r.Context()
	preq := r.Clone(ctx)

	preq.Header.Add(RequestHeaderRequestID, uuidStr)

	streamValue, streamOk := completionRequest[RequestFieldStream]
	streamOptionsValue, streamOptionsOk := completionRequest[RequestFieldStreamOptions]

	completionRequest[RequestFieldStream] = false

	kvParams := map[string]any{}
	kvParams[RequestFieldKVDoRemoteDecode] = true
	completionRequest[RequestFieldKVTransferParams] = kvParams

	delete(completionRequest, RequestFieldStreamOptions)

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
		if err := errorBadGateway(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}

	// 2. Forward request to prefiller
	s.logger.Info("sending request to prefiller", "url", prefillPodURL, "body", string(pbody))
	pw := &bufferedResponseWriter{}
	prefillHandler.ServeHTTP(pw, preq)

	if pw.statusCode < 200 || pw.statusCode >= 300 {
		s.logger.Error(err, "request failed", "code", pw.statusCode)
		w.WriteHeader(pw.statusCode)
		return
	}

	// Process response - extract p/d fields
	var prefillerResponse map[string]any
	if err := json.Unmarshal([]byte(pw.buffer.String()), &prefillerResponse); err != nil {
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}

	// 1. Verify fields exists

	kvParamsPrefillRaw, ok := prefillerResponse[RequestFieldKVTransferParams]
	if !ok {
		s.logger.Info("warning: missing 'kv_transfer_params' section in prefiller response")
		return
	}

	kvParamsPrefill, ok := kvParamsPrefillRaw.(map[string]any)
	if !ok {
		s.logger.Info("warning: 'kv_transfer_params' field is not a valid object in prefiller response")
		return
	}

	blockIDs, ok := kvParamsPrefill[RequestFieldKVRemoteBlockIDs]
	if !ok {
		s.logger.Info("warning: missing 'remote_block_ids' field in kv_transfer_params in prefiller response")
	}

	engineID, ok := kvParamsPrefill[RequestFieldKVRemoteEngineID]
	if !ok {
		// TODO: error or ignore?
		s.logger.Info("warning: missing 'remote_engine_id' field in kv_transfer_params in prefiller response")
	}

	remoteHost, ok := kvParamsPrefill[RequestFieldKVRemoteHost]
	if !ok {
		// TODO: error or ignore?
		s.logger.Info("warning: missing 'remote_host' field in kv_transfer_params in prefiller response")
	}

	remotePort, ok := kvParamsPrefill[RequestFieldKVRemotePort]
	if !ok {
		// TODO: error or ignore?
		s.logger.Info("warning: missing 'remote_port' field in kv_transfer_params in prefiller response")
	}

	// Log what we got
	s.logger.Info("received prefiller response",
		RequestFieldKVRemoteBlockIDs, blockIDs,
		RequestFieldKVRemoteEngineID, engineID,
		RequestFieldKVRemoteHost, remoteHost,
		RequestFieldKVRemotePort, remotePort,
	)

	// 2. Prepare decode request
	dreq := r.Clone(ctx)

	dreq.Header.Add(RequestHeaderRequestID, uuidStr)

	delete(kvParams, RequestFieldKVDoRemoteDecode)
	delete(completionRequest, RequestFieldStream)
	if streamOk {
		completionRequest[RequestFieldStream] = streamValue
	}
	if streamOptionsOk {
		completionRequest[RequestFieldStreamOptions] = streamOptionsValue
	}

	kvParams[RequestFieldKVDoRemotePrefill] = true
	kvParams[RequestFieldKVRemoteBlockIDs] = blockIDs
	kvParams[RequestFieldKVRemoteEngineID] = engineID
	kvParams[RequestFieldKVRemoteHost] = remoteHost
	kvParams[RequestFieldKVRemotePort] = remotePort

	dbody, err := json.Marshal(completionRequest)
	if err != nil {
		if err := errorJSONInvalid(err, w); err != nil {
			s.logger.Error(err, "failed to send error response to client")
		}
		return
	}
	dreq.Body = io.NopCloser(strings.NewReader(string(dbody)))
	dreq.ContentLength = int64(len(dbody))

	// 3. Forward to local decoder.
	s.logger.Info("sending request to decoder", "body", string(dbody))
	s.decoderProxy.ServeHTTP(w, dreq)
}
