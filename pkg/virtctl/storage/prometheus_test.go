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
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	"k8s.io/client-go/rest"
)

func testMonitoringConfig() *rest.Config {
	return &rest.Config{Host: "https://api.example.test", BearerToken: "test-token"}
}

func testPrometheusSource(endpoint string) *prometheusSource {
	return newPrometheusSource(endpoint, "", testMonitoringConfig)
}

func writeVector(w http.ResponseWriter, vector model.Vector) {
	w.Header().Set("Content-Type", "application/json")
	Expect(json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "data": map[string]interface{}{"resultType": "vector", "result": vector}})).To(Succeed())
}

var _ = Describe("Prometheus source", func() {
	DescribeTable("validate monitoring base URLs", func(endpoint, expected string) {
		api, err := newPrometheusAPI(endpoint, "", testMonitoringConfig)
		if expected == "" {
			Expect(err).NotTo(HaveOccurred())
			Expect(api).NotTo(BeNil())
		} else {
			Expect(err).To(MatchError(ContainSubstring(expected)))
		}
	},
		Entry("explicit HTTPS endpoint", "https://monitor.example.test", ""),
		Entry("explicit loopback endpoint", "http://127.0.0.1:19091", ""),
		Entry("missing URL", "", "--prometheus-url is required to read monitoring metrics"),
		Entry("missing scheme", "monitor.example.test", "must be an HTTP(S) base URL"),
		Entry("unsupported scheme", "ftp://monitor.example.test", "must be an HTTP(S) base URL"),
		Entry("embedded credentials", "https://user:secret@monitor.example.test", "must be an HTTP(S) base URL"),
		Entry("query parameters", "https://monitor.example.test?token=secret", "must be an HTTP(S) base URL"),
		Entry("fragment", "https://monitor.example.test/#fragment", "must be an HTTP(S) base URL"),
		Entry("non-loopback HTTP", "http://monitor.example.test", "only on loopback"),
	)

	DescribeTable("use monitoring TLS trust and bearer authentication", func(trusted bool) {
		received := []string{}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			received = append(received, r.Header.Get("Authorization"))
			writeVector(w, model.Vector{})
		}))
		defer server.Close()
		var ca string
		if trusted {
			ca = filepath.Join(GinkgoT().TempDir(), "monitoring-ca.pem")
			Expect(os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)).To(Succeed())
		}
		source := newPrometheusSource(server.URL, ca, testMonitoringConfig)
		_, err := source.getAPI()
		Expect(err).NotTo(HaveOccurred())
		_, err = source.queryVector(context.Background(), "up", "up", time.Now())
		if trusted {
			Expect(err).NotTo(HaveOccurred())
			Expect(received).To(Equal([]string{"Bearer test-token"}))
		} else {
			Expect(err).To(HaveOccurred())
			Expect(received).To(BeEmpty())
		}
	}, Entry("trusted monitoring certificate", true), Entry("untrusted monitoring certificate", false))

	It("does not follow a redirect to another endpoint", func() {
		calls := 0
		destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
		defer destination.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
		defer source.Close()
		monitoring := testPrometheusSource(source.URL)
		_, err := monitoring.getAPI()
		Expect(err).NotTo(HaveOccurred())
		_, err = monitoring.queryVector(context.Background(), "up", "up", time.Now())
		Expect(err).To(HaveOccurred())
		Expect(calls).To(BeZero())
	})
})
