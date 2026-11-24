/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 */

package storage

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"k8s.io/client-go/rest"
)

const (
	prometheusURLFlag = "prometheus-url"
	prometheusCAFlag  = "prometheus-ca"
	monitoringTimeout = 15 * time.Second
)

type prometheusSource struct {
	getAPI func() (promv1.API, error)
}

func newPrometheusSource(endpoint, caFile string, clientConfig func() *rest.Config) *prometheusSource {
	// Build the client on the first query and share it for the rest of the inspection.
	return &prometheusSource{getAPI: sync.OnceValues(func() (promv1.API, error) {
		return newPrometheusAPI(endpoint, caFile, clientConfig)
	})}
}

func (p *prometheusSource) queryVector(ctx context.Context, metric, query string, at time.Time) (model.Vector, error) {
	api, err := p.getAPI()
	if err != nil {
		return nil, err
	}
	result, warnings, err := api.Query(ctx, query, at)
	if err != nil {
		return nil, fmt.Errorf("cannot query %s: %w", metric, err)
	}
	if len(warnings) > 0 {
		return nil, fmt.Errorf("incomplete monitoring result for %s: %v", metric, warnings)
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected monitoring result for %s: expected an instant vector", metric)
	}
	return vector, nil
}

func newPrometheusAPI(endpoint, caFile string, clientConfig func() *rest.Config) (promv1.API, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("--%s is required to read monitoring metrics", prometheusURLFlag)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("--%s must be an HTTP(S) base URL without credentials, query or fragment", prometheusURLFlag)
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, fmt.Errorf("unencrypted monitoring URLs are supported only on loopback addresses")
	}
	original := clientConfig()
	if original == nil {
		return nil, fmt.Errorf("Kubernetes client configuration is unavailable")
	}
	config := rest.CopyConfig(original)
	if u.Scheme == "http" {
		// Local forwarded/test endpoints use anonymous HTTP; never send cluster credentials in clear text.
		config = rest.AnonymousClientConfig(config)
	}
	config.Host = endpoint
	config.Timeout = monitoringTimeout
	config.Transport = nil
	config.WrapTransport = nil
	// The monitoring endpoint has its own TLS identity. Do not reuse the API server CA,
	// client certificate, server name override or insecure-skip-TLS-verify setting.
	config.TLSClientConfig = rest.TLSClientConfig{CAFile: caFile}
	client, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("cannot configure monitoring connection: %w", err)
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	promClient, err := promapi.NewClient(promapi.Config{Address: endpoint, Client: client})
	if err != nil {
		return nil, fmt.Errorf("cannot configure monitoring API: %w", err)
	}
	return promv1.NewAPI(promClient), nil
}
