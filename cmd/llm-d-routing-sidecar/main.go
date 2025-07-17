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
	"os"

	"k8s.io/klog/v2"

	"github.com/llm-d/llm-d-routing-sidecar/internal/proxy"
	"github.com/llm-d/llm-d-routing-sidecar/internal/signals"
)

func main() {
	var (
		port                   string
		vLLMPort               string
		connector              string
		prefillerUseTLS        bool
		enableSSRFProtection   bool
		inferencePoolNamespace string
		inferencePoolName      string
	)

	flag.StringVar(&port, "port", "8000", "the port the sidecar is listening on")
	flag.StringVar(&vLLMPort, "vllm-port", "8001", "the port vLLM is listening on")
	flag.StringVar(&connector, "connector", "nixl", "the P/D connector being used. Either nixl, nixlv2 or lmcache")
	flag.BoolVar(&prefillerUseTLS, "prefiller-use-tls", false, "whether to use TLS when sending requests to prefillers")
	flag.BoolVar(&enableSSRFProtection, "enable-ssrf-protection", false, "enable SSRF protection using InferencePool allowlisting")
	flag.StringVar(&inferencePoolNamespace, "inference-pool-namespace", "", "the Kubernetes namespace to watch for InferencePool resources (defaults to INFERENCE_POOL_NAMESPACE env var)")
	flag.StringVar(&inferencePoolName, "inference-pool-name", "", "the specific InferencePool name to watch (defaults to INFERENCE_POOL_NAME env var)")
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

	// Determine namespace and pool name for SSRF protection
	if enableSSRFProtection {
		// Priority: command line flag > environment variable
		if inferencePoolNamespace == "" {
			inferencePoolNamespace = os.Getenv("INFERENCE_POOL_NAMESPACE")
		}
		if inferencePoolName == "" {
			inferencePoolName = os.Getenv("INFERENCE_POOL_NAME")
		}

		if inferencePoolNamespace == "" {
			logger.Info("Error: --inference-pool-namespace or INFERENCE_POOL_NAMESPACE environment variable is required when --enable-ssrf-protection is true")
			return
		}
		if inferencePoolName == "" {
			logger.Info("Error: --inference-pool-name or INFERENCE_POOL_NAME environment variable is required when --enable-ssrf-protection is true")
			return
		}

		logger.Info("SSRF protection enabled", "namespace", inferencePoolNamespace, "poolName", inferencePoolName)
	}

	// start reverse proxy HTTP server
	targetURL, err := url.Parse("http://localhost:" + vLLMPort)
	if err != nil {
		logger.Error(err, "Failed to create targetURL")
		return
	}

	proxy, err := proxy.NewProxy(port, targetURL, connector, prefillerUseTLS, enableSSRFProtection, inferencePoolNamespace, inferencePoolName)
	if err != nil {
		logger.Error(err, "Failed to create proxy")
		return
	}
	if err := proxy.Start(ctx); err != nil {
		logger.Error(err, "Failed to start proxy server")
	}
}
