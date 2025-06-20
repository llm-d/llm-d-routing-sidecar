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
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
)

const (
	requestHeaderPrefillURL = "x-prefiller-url"
	requestHeaderRequestID  = "x-request-id"

	requestFieldKVTransferParams = "kv_transfer_params"
	requestFieldMaxTokens        = "max_tokens"
	requestFieldDoRemotePrefill  = "do_remote_prefill"
	requestFieldDoRemoteDecode   = "do_remote_decode"
	requestFieldRemoteBlockIDs   = "remote_block_ids"
	requestFieldRemoteEngineID   = "remote_engine_id"
	requestFieldRemoteHost       = "remote_host"
	requestFieldRemotePort       = "remote_port"
	requestFieldStream           = "stream"
	requestFieldStreamOptions    = "stream_options"

	// ConnectorNIXLV1 enables the (now deprecated) P/D NIXL v1 protocol
	ConnectorNIXLV1 = "nixl"

	// ConnectorNIXLV2 enables the P/D NIXL v2 protocol
	ConnectorNIXLV2 = "nixlv2"

	// ConnectorLMCache enables (now deprecated) P/D LMCache protocol
	ConnectorLMCache = "lmcache"
)

type protocolRunner func(http.ResponseWriter, *http.Request, string)

// Server is the reverse proxy server
type Server struct {
	logger               logr.Logger
	addr                 net.Addr       // the proxy TCP address
	port                 string         // the proxy TCP port
	decoderURL           *url.URL       // the local decoder URL
	decoderProxy         http.Handler   // decoder proxy handler
	runConnectorProtocol protocolRunner // the handler for running the protocol

	prefillerProxies   map[string]http.Handler // cached prefiller proxy handlers
	prefillerProxiesMu sync.RWMutex
}

// NewProxy creates a new routing reverse proxy
func NewProxy(port string, decodeURL *url.URL, connector string) *Server {
	server := &Server{
		port:             port,
		decoderURL:       decodeURL,
		prefillerProxies: make(map[string]http.Handler),
	}
	switch connector {
	case ConnectorLMCache:
		server.runConnectorProtocol = server.runLMCacheProtocol
	case ConnectorNIXLV1:
		server.runConnectorProtocol = server.runNIXLProtocolV1
	case ConnectorNIXLV2:
		fallthrough
	default:
		server.runConnectorProtocol = server.runNIXLProtocolV2
	}

	return server
}

// Start the HTTP reverse proxy.
func (s *Server) Start(ctx context.Context) error {
	logger := klog.FromContext(ctx).WithName("proxy server")
	s.logger = logger

	ln, err := net.Listen("tcp", ":"+s.port)
	if err != nil {
		logger.Error(err, "Failed to start")
		return err
	}
	s.addr = ln.Addr()

	// Configure handlers
	mux := s.createRoutes()

	server := &http.Server{Handler: mux}

	// Setup graceful termination (not strictly needed for sidecars)
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")

		ctx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error(err, "Failed to gracefully shutdown")
		}
	}()

	logger.Info("starting", "addr", s.addr.String())
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Error(err, "Failed to start")
		return err
	}

	return nil
}

func (s *Server) createRoutes() *http.ServeMux {
	// Configure handlers
	mux := http.NewServeMux()

	// Intercept chat requests
	mux.HandleFunc("POST "+ChatCompletionsPath, s.chatCompletionsHandler) // /v1/chat/completions (openai)
	mux.HandleFunc("POST "+CompletionsPath, s.chatCompletionsHandler)     // /v1/completions (legacy)

	// Passthrough decoder handler
	s.decoderProxy = httputil.NewSingleHostReverseProxy(s.decoderURL)
	mux.Handle("/", s.decoderProxy)

	return mux
}

func (s *Server) prefillerProxyHandler(targetURL string) (http.Handler, error) {
	s.prefillerProxiesMu.RLock()
	proxy, exists := s.prefillerProxies[targetURL]
	s.prefillerProxiesMu.RUnlock()

	if exists {
		return proxy, nil
	}

	u, err := url.Parse(targetURL)
	if err != nil {
		s.logger.Error(err, "failed to parse URL", "url", targetURL)
		return nil, err
	}
	proxy = httputil.NewSingleHostReverseProxy(u)

	s.prefillerProxiesMu.Lock()
	s.prefillerProxies[targetURL] = proxy
	s.prefillerProxiesMu.Unlock()

	return proxy, nil
}
