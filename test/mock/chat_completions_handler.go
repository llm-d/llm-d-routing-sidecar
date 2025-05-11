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

package mock

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// ChatCompletion is a simple chat completion mock handler
type ChatCompletionHandler struct {
	Connector           string
	RequestCount        atomic.Int32
	CompletionRequests  []map[string]any
	CompletionResponses []map[string]any
	mu                  sync.Mutex
}

func (cc *ChatCompletionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cc.RequestCount.Add(1)

	defer r.Body.Close() //nolint:all
	b, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest) // TODO: check FastAPI error code when failing to read body
		w.Write([]byte(err.Error()))         //nolint:all
		return
	}

	var completionRequest map[string]any
	if err := json.Unmarshal(b, &completionRequest); err != nil {
		w.Write([]byte(err.Error())) //nolint:all
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if cc.Connector == "nixl" {
		raw := []byte(`{"kv_transfer_params": {"remote_block_ids":[1, 2, 3], "remote_engine_id": "5b5fb28f-3f30-4bdd-9a36-958d52459200"}}`)

		var completionResponse map[string]any
		if err := json.Unmarshal(raw, &completionResponse); err != nil {
			w.Write([]byte(err.Error())) //nolint:all
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		cc.mu.Lock()
		cc.CompletionResponses = append(cc.CompletionResponses, completionResponse)
		cc.mu.Unlock()

		w.Write(raw) //nolint:all
	}

	cc.mu.Lock()
	cc.CompletionRequests = append(cc.CompletionRequests, completionRequest)
	cc.mu.Unlock()

	w.WriteHeader(200)
}
