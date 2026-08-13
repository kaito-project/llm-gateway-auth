// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package azure

import (
	"errors"
	"flag"
	"os"
	"time"
)

// Config holds all configuration for the ext_authz server.
type Config struct {
	// gRPC server
	GRPCAddr    string
	MetricsAddr string

	// Kubernetes
	KubeconfigPath string

	// SAR resource mapping
	SARAPIGroup string // API group for ResourceAttributes (e.g. "aimanager")
	SARResource string // resource type for ResourceAttributes (e.g. "namespaces")
	SARVerb     string // verb for ResourceAttributes (e.g. "inference")

	// Behavior
	FailOpen       bool
	RequestTimeout time.Duration
	CacheTTL       time.Duration
	CacheSize      int

	// TLS
	TLSCertFile string
	TLSKeyFile  string
}

// ParseFlags parses CLI flags and overlays environment variables.
// Environment variables take the form EXTAUTHZ_<UPPER_SNAKE_CASE>.
// An explicit flag value always wins over the environment variable.
func ParseFlags() (*Config, error) {
	c := &Config{}

	flag.StringVar(&c.GRPCAddr, "grpc-addr", ":9001", "gRPC listen address")
	flag.StringVar(&c.MetricsAddr, "metrics-addr", ":9090", "HTTP metrics listen address")
	flag.StringVar(&c.KubeconfigPath, "kubeconfig", "", "Path to kubeconfig (in-cluster if empty)")
	flag.StringVar(&c.SARAPIGroup, "sar-api-group", "aimanager", "API group for SubjectAccessReview ResourceAttributes")
	flag.StringVar(&c.SARResource, "sar-resource", "namespaces", "Resource type for SubjectAccessReview ResourceAttributes")
	flag.StringVar(&c.SARVerb, "sar-verb", "inference", "Verb for SubjectAccessReview ResourceAttributes")
	flag.BoolVar(&c.FailOpen, "fail-open", false, "Allow requests when authorization infrastructure is unavailable")
	flag.DurationVar(&c.RequestTimeout, "request-timeout", 5*time.Second, "Per-request timeout for Check calls")
	flag.DurationVar(&c.CacheTTL, "cache-ttl", 5*time.Minute, "SubjectAccessReview cache TTL")
	flag.IntVar(&c.CacheSize, "cache-size", 1024, "SubjectAccessReview LRU cache max entries")
	flag.StringVar(&c.TLSCertFile, "tls-cert-file", "", "TLS certificate file path")
	flag.StringVar(&c.TLSKeyFile, "tls-key-file", "", "TLS private key file path")

	flag.Parse()

	applyEnvOverrides(c)

	if err := validate(c); err != nil {
		return nil, err
	}

	return c, nil
}

// applyEnvOverrides overlays environment variables onto any flags that
// were not explicitly set on the command line.
func applyEnvOverrides(c *Config) {
	envOverride(&c.GRPCAddr, "EXTAUTHZ_GRPC_ADDR")
	envOverride(&c.MetricsAddr, "EXTAUTHZ_METRICS_ADDR")
	envOverride(&c.KubeconfigPath, "KUBECONFIG")
	envOverride(&c.SARAPIGroup, "EXTAUTHZ_SAR_API_GROUP")
	envOverride(&c.SARResource, "EXTAUTHZ_SAR_RESOURCE")
	envOverride(&c.SARVerb, "EXTAUTHZ_SAR_VERB")
	envOverride(&c.TLSCertFile, "EXTAUTHZ_TLS_CERT_FILE")
	envOverride(&c.TLSKeyFile, "EXTAUTHZ_TLS_KEY_FILE")

	if v := os.Getenv("EXTAUTHZ_FAIL_OPEN"); v == "true" {
		c.FailOpen = true
	}
}

// envOverride sets *dst to the value of environment variable key
// if *dst is empty and the environment variable is set.
func envOverride(dst *string, key string) {
	if *dst == "" {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
}

// validate checks that all required configuration is present and
// derives computed fields.
func validate(c *Config) error {
	// Validate TLS config: both or neither must be set.
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("--tls-cert-file and --tls-key-file must both be set or both be empty")
	}

	if c.CacheSize <= 0 {
		return errors.New("--cache-size must be positive")
	}
	if c.CacheTTL <= 0 {
		return errors.New("--cache-ttl must be positive")
	}
	if c.RequestTimeout <= 0 {
		return errors.New("--request-timeout must be positive")
	}

	return nil
}
