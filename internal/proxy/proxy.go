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
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	lru "github.com/hashicorp/golang-lru/v2"
	"k8s.io/klog/v2"
)

const (
	requestHeaderPrefillURL      = "x-prefiller-url"
	requestHeaderPrefillHostPort = "x-prefiller-host-port"
	requestHeaderRequestID       = "x-request-id"

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
	prefillerURLPrefix   string

	prefillerProxies *lru.Cache[string, http.Handler] // cached prefiller proxy handlers
}

// NewProxy creates a new routing reverse proxy
func NewProxy(port string, decodeURL *url.URL, connector string, prefillerUseTLS bool) *Server {
	cache, _ := lru.New[string, http.Handler](16) // nolint:all

	server := &Server{
		port:               port,
		decoderURL:         decodeURL,
		prefillerProxies:   cache,
		prefillerURLPrefix: "http://",
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

	if prefillerUseTLS {
		server.prefillerURLPrefix = "https://"
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
	decoderProxy := httputil.NewSingleHostReverseProxy(s.decoderURL)
	decoderProxy.ErrorHandler = func(res http.ResponseWriter, _ *http.Request, err error) {

		// Log errors from the decoder proxy
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			s.logger.Error(err, "waiting for vLLM to be ready")
		default:
			s.logger.Error(err, "http: proxy error")
		}
		res.WriteHeader(http.StatusBadGateway)
	}
	s.decoderProxy = decoderProxy
	mux.Handle("/", s.decoderProxy)

	return mux
}

func (s *Server) prefillerProxyHandler(hostPort string) (http.Handler, error) {
	proxy, exists := s.prefillerProxies.Get(hostPort)
	if exists {
		return proxy, nil
	}

	// Backward compatible behavior: trim `http:` prefix
	hostPort, _ = strings.CutPrefix(hostPort, "http://")

	u, err := url.Parse(s.prefillerURLPrefix + hostPort)
	if err != nil {
		s.logger.Error(err, "failed to parse URL", "hostPort", hostPort)
		return nil, err
	}

	// check if the prefill target IP is "reasonable"
	targetIP := net.ParseIP(u.Host)
	if targetIP == nil || !isPrivateOrSpecialIP(targetIP) {
		s.logger.Error(err, "invalid host", "host", targetIP.String())
		return nil, err
	}

	proxy = httputil.NewSingleHostReverseProxy(u)
	s.prefillerProxies.Add(hostPort, proxy)

	return proxy, nil
}

// Determine if the provided IP address is in a private (or non-Internet-routable)
// range. A safer approach would be to validate that it is contained within the
// cluster's PodCIDR. Unfortunately, there's no standard k8s API for it (i.e.,
// it is distribution and CNI dependant).
//
// Alternative options could be to
// (1) receive via a command line parameter (shift the responsibility to the
// operator); or
// (2) validate against all nodes IP ranges (which requires elevated API access
// privileges).
// Neither option is attractive :-(
func isPrivateOrSpecialIP(ip net.IP) bool {
	ip = ip.To4() // ensure we only evaluate IPv4 addresses at this time
	if ip == nil {
		return false
	}

	for _, cidr := range specialCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// Allowed CIDRs
var (
	specialCIDRs []*net.IPNet // the allowed subnets

	cidrStrings = []string{ // special or private network ranges
		"10.0.0.0/8",         // private (from rfc 1918)
		"172.16.0.0/12",      // private (from rfc 1918)
		"192.168.0.0/16",     // private (from rfc 1918)
		"127.0.0.0/8",        // loopback
		"169.254.0.0/16",     // link-local
		"100.64.0.0/10",      // carrier-grade NAT
		"192.0.0.0/24",       // protocol assignments (IETF)
		"192.0.2.0/24",       // test network
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // test network
		"203.0.113.0/24",     // test network
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved
		"0.0.0.0/8",          // "this" network
		"255.255.255.255/32", // broadcast address
	}
)

func init() { // populate the special CIDR array from the subnet strings
	for _, cidr := range cidrStrings {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			specialCIDRs = append(specialCIDRs, network)
		}
	}
}
