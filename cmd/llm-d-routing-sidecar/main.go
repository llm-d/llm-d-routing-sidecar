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
package main

import (
	"context"
	"flag"
	"net/url"

	"k8s.io/klog/v2"

	"github.com/llm-d/llm-d-routing-sidecar/internal/proxy"
	"github.com/llm-d/llm-d-routing-sidecar/internal/signals"
)

func main() {
	var (
		port      string
		vLLMPort  string
		connector string
	)

	flag.StringVar(&port, "port", "8000", "the port the sidecar is listening on")
	flag.StringVar(&vLLMPort, "vllm-port", "8001", "the port vLLM is listening on")
	flag.StringVar(&connector, "connector", "nixl", "the P/D connector being used. Either nixl, nixlv2 or lmcache")
	klog.InitFlags(nil)
	flag.Parse()

	// make sure to flush logs before exiting
	defer klog.Flush()

	ctx := signals.SetupSignalHandler(context.Background())
	logger := klog.FromContext(ctx)

	if connector != proxy.ConnectorNIXLV1 && connector != proxy.ConnectorNIXLV2 && connector != proxy.ConnectorLMCache {
		logger.Info("Error: --connector must either be 'nixl', 'nixlv2' or 'lmcache'")
		return
	}
	logger.Info("p/d connector validated", "connector", connector)

	// start reverse proxy HTTP server
	targetURL, err := url.Parse("http://localhost:" + vLLMPort)
	if err != nil {
		logger.Error(err, "Failed to create targetURL")
		return
	}

	proxy := proxy.NewProxy(port, targetURL, connector)
	if err := proxy.Start(ctx); err != nil {
		logger.Error(err, "Failed to start proxy server")
	}
}
