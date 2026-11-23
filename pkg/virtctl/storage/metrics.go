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
	"math"
	"net"
	"net/http"
	"net/url"
	"text/tabwriter"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	v1 "kubevirt.io/api/core/v1"
)

const (
	IOPSRead                 = "iops_read_total"
	IOPSWrite                = "iops_write_total"
	ReadTrafficBytes         = "read_traffic_bytes_total"
	WriteTrafficBytes        = "write_traffic_bytes_total"
	ReadTimesSeconds         = "read_times_seconds_total"
	WriteTimesSeconds        = "write_times_seconds_total"
	FlushRequests            = "flush_requests_total"
	FlushTimesSeconds        = "flush_times_seconds_total"
	kubevirtVmiStoragePrefix = "kubevirt_vmi_storage_"
	namespaceLabel           = "namespace"
	nameLabel                = "name"
	nodeLabel                = "node"
	driveLabel               = "drive"
	prometheusURLFlag        = "prometheus-url"
	prometheusCAFlag         = "prometheus-ca"
	monitoringWindow         = 5 * time.Minute
	monitoringMaxAge         = 2 * time.Minute
	monitoringTimeout        = 15 * time.Second
	bytesPerMiB              = 1024 * 1024
	millisecondsPerSecond    = 1000
	domainColumnCount        = 8
	metricValueFormat        = "%.2f"
	noCompletedOperations    = "-"
)

type diskRates map[string]float64
type domainRates map[string]diskRates

func (s *storageCommand) printDomainPerformance(cmd *cobra.Command, vmi *v1.VirtualMachineInstance, disks []diskInfo) error {
	if len(disks) == 0 {
		return nil
	}
	if vmi == nil || !vmi.IsRunning() || vmi.Status.NodeName == "" {
		cmd.Println("\nDomain performance: unavailable (VMI is not running on a node).")
		return nil
	}
	at := time.Now()
	window := domainWindow(vmi, at)
	rates, err := s.queryDomain(cmd.Context(), vmi, at, window)
	if err != nil {
		if cmd.Context().Err() != nil {
			return cmd.Context().Err()
		}
		cmd.PrintErrf("Warning: Domain metrics unavailable: %v\n", err)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if err == nil {
		fmt.Fprintf(w, "\nDomain performance (Prometheus, lookback %s, evaluated at %s):\n", window, at.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintln(w, "\nDomain performance (Prometheus): unavailable")
	}
	fmt.Fprintln(w, "DISK\tREAD IOPS\tWRITE IOPS\tREAD MiB/s\tWRITE MiB/s\tREAD LATENCY ms\tWRITE LATENCY ms\tFLUSH OPS/s\tFLUSH LATENCY ms")
	for _, disk := range disks {
		values := diskPerformance(rates[disk.name])
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", disk.name, values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7])
	}
	return w.Flush()
}

// Keep the lookback inside the current VMI's known lifetime and running segment.
// Metric labels do not include a VMI UID, so a reused name must not select an older lifetime.
func domainWindow(vmi *v1.VirtualMachineInstance, at time.Time) time.Duration {
	start := at.Add(-monitoringWindow)
	if vmi.CreationTimestamp.Time.After(start) {
		start = vmi.CreationTimestamp.Time
	}
	for _, transition := range vmi.Status.PhaseTransitionTimestamps {
		if transition.Phase == v1.Running && transition.PhaseTransitionTimestamp.Time.After(start) {
			start = transition.PhaseTransitionTimestamp.Time
		}
	}
	if migration := vmi.Status.MigrationState; migration != nil && migration.Completed && migration.EndTimestamp != nil && migration.EndTimestamp.Time.After(start) {
		start = migration.EndTimestamp.Time
	}
	return at.Sub(start).Truncate(time.Second)
}

func (s *storageCommand) queryDomain(ctx context.Context, vmi *v1.VirtualMachineInstance, at time.Time, window time.Duration) (domainRates, error) {
	if state := vmi.Status.MigrationState; state != nil && !state.Completed {
		return nil, fmt.Errorf("VMI migration is in progress; retry after completion")
	}
	if window < time.Second {
		return nil, fmt.Errorf("the current VMI running period has insufficient metric history")
	}
	ctx, cancel := context.WithTimeout(ctx, monitoringTimeout)
	defer cancel()
	api, err := s.monitoringAPI()
	if err != nil {
		return nil, err
	}
	rates := domainRates{}
	for _, metric := range []string{IOPSRead, IOPSWrite, ReadTrafficBytes, WriteTrafficBytes, ReadTimesSeconds, WriteTimesSeconds, FlushRequests, FlushTimesSeconds} {
		result, warnings, err := api.Query(ctx, domainQuery(metric, vmi, window), at)
		if err != nil {
			return nil, fmt.Errorf("cannot query %s: %w", kubevirtVmiStoragePrefix+metric, err)
		}
		if len(warnings) > 0 {
			return nil, fmt.Errorf("incomplete monitoring result for %s: %v", metric, warnings)
		}
		vector, ok := result.(model.Vector)
		if !ok {
			return nil, fmt.Errorf("unexpected monitoring result for %s: expected an instant vector", metric)
		}
		if err := addDomainRates(rates, vector, metric, vmi); err != nil {
			return nil, err
		}
	}
	if len(rates) == 0 {
		return nil, fmt.Errorf("no recent storage rates for VMI %s/%s on node %s; check metric collection and available history", vmi.Namespace, vmi.Name, vmi.Status.NodeName)
	}
	return rates, nil
}

func domainQuery(metric string, vmi *v1.VirtualMachineInstance, window time.Duration) string {
	selector := fmt.Sprintf("%s%s{%s=%q,%s=%q,%s=%q,%s!=\"\"}", kubevirtVmiStoragePrefix, metric,
		namespaceLabel, vmi.Namespace, nameLabel, vmi.Name, nodeLabel, vmi.Status.NodeName, driveLabel)
	// Require a recent stored sample without depending on one scrape containing the series.
	return fmt.Sprintf("rate(%s[%ds]) and present_over_time(%s[%ds])", selector, int64(window.Seconds()), selector, int64(monitoringMaxAge.Seconds()))
}

func (s *storageCommand) monitoringAPI() (promv1.API, error) {
	endpoint := s.prometheusURL
	if endpoint == "" {
		return nil, fmt.Errorf("--%s is required to read Domain metrics", prometheusURLFlag)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("--%s must be an HTTP(S) base URL without credentials, query or fragment", prometheusURLFlag)
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, fmt.Errorf("unencrypted monitoring URLs are supported only on loopback addresses")
	}
	original := s.client.Config()
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
	config.TLSClientConfig = rest.TLSClientConfig{CAFile: s.prometheusCA}
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

func addDomainRates(rates domainRates, vector model.Vector, metric string, vmi *v1.VirtualMachineInstance) error {
	for _, sample := range vector {
		labels := sample.Metric
		drive := string(labels[driveLabel])
		if labels[namespaceLabel] != model.LabelValue(vmi.Namespace) || labels[nameLabel] != model.LabelValue(vmi.Name) || labels[nodeLabel] != model.LabelValue(vmi.Status.NodeName) || drive == "" {
			continue
		}
		value := float64(sample.Value)
		if sample.Histogram != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("invalid monitoring rate %s for disk %s", metric, drive)
		}
		if rates[drive] == nil {
			rates[drive] = diskRates{}
		}
		if _, duplicate := rates[drive][metric]; duplicate {
			return fmt.Errorf("ambiguous monitoring rate %s for disk %s", metric, drive)
		}
		rates[drive][metric] = value
	}
	return nil
}

func diskPerformance(rates diskRates) [domainColumnCount]string {
	values := [domainColumnCount]string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable}
	for i, metrics := range [][3]string{{IOPSRead, ReadTrafficBytes, ReadTimesSeconds}, {IOPSWrite, WriteTrafficBytes, WriteTimesSeconds}} {
		operations, hasOperations := rates[metrics[0]]
		traffic, hasTraffic := rates[metrics[1]]
		seconds, hasSeconds := rates[metrics[2]]
		if hasOperations {
			values[i] = fmt.Sprintf(metricValueFormat, operations)
		}
		if hasTraffic {
			values[i+2] = fmt.Sprintf(metricValueFormat, traffic/bytesPerMiB)
		}
		if hasOperations && hasSeconds {
			values[i+4] = noCompletedOperations
			if operations > 0 {
				values[i+4] = fmt.Sprintf(metricValueFormat, seconds/operations*millisecondsPerSecond)
			}
		}
	}
	if operations, exists := rates[FlushRequests]; exists {
		values[6] = fmt.Sprintf(metricValueFormat, operations)
		if seconds, exists := rates[FlushTimesSeconds]; exists {
			values[7] = noCompletedOperations
			if operations > 0 {
				values[7] = fmt.Sprintf(metricValueFormat, seconds/operations*millisecondsPerSecond)
			}
		}
	}
	return values
}
