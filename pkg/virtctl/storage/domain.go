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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/prometheus/common/model"
	"github.com/spf13/cobra"
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
	driveLabel               = "drive"
	bytesPerMiB              = 1024 * 1024
	millisecondsPerSecond    = 1000
	noCompletedOperations    = "-"
)

type domainLevel struct {
	source *prometheusSource
}

type diskRates map[string]float64
type domainRates map[string]diskRates

func (d domainLevel) print(cmd *cobra.Command, vmi *v1.VirtualMachineInstance, disks []diskInfo) error {
	if len(disks) == 0 {
		return nil
	}
	if !vmiRunningOnNode(vmi) {
		cmd.Println("\nDomain performance: unavailable (VMI is not running on a node).")
		return nil
	}
	at := time.Now()
	window := vmiMetricsWindow(vmi, at)
	rates, err := d.query(cmd.Context(), vmi, at, window)
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
	fmt.Fprintln(w, "DISK\tREAD IOPS\tWRITE IOPS\tTOTAL IOPS\tREAD MiB/s\tWRITE MiB/s\tREAD LATENCY ms\tWRITE LATENCY ms\tTOTAL LATENCY ms\tFLUSH OPS/s\tFLUSH LATENCY ms")
	for _, disk := range disks {
		values := diskPerformance(rates[disk.name])
		fmt.Fprintf(w, "%s\t%s\n", disk.name, strings.Join(values, "\t"))
	}
	return w.Flush()
}

func (d domainLevel) query(ctx context.Context, vmi *v1.VirtualMachineInstance, at time.Time, window time.Duration) (domainRates, error) {
	if err := checkMetricMigration(vmi); err != nil {
		return nil, err
	}
	if window < time.Second {
		return nil, fmt.Errorf("the current VMI running period has insufficient metric history")
	}
	ctx, cancel := context.WithTimeout(ctx, monitoringTimeout)
	defer cancel()
	rates := domainRates{}
	for _, metric := range []string{IOPSRead, IOPSWrite, ReadTrafficBytes, WriteTrafficBytes, ReadTimesSeconds, WriteTimesSeconds, FlushRequests, FlushTimesSeconds} {
		vector, err := d.source.queryVector(ctx, kubevirtVmiStoragePrefix+metric, domainQuery(metric, vmi, window), at)
		if err != nil {
			return nil, err
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
	selector := fmt.Sprintf("%s%s{%s,%s!=\"\"}", kubevirtVmiStoragePrefix, metric, vmiMetricSelector(vmi), driveLabel)
	// Require a recent stored sample without depending on one scrape containing the series.
	return fmt.Sprintf("rate(%s[%ds]) and present_over_time(%s[%ds])", selector, int64(window.Seconds()), selector, int64(monitoringMaxAge.Seconds()))
}

func addDomainRates(rates domainRates, vector model.Vector, metric string, vmi *v1.VirtualMachineInstance) error {
	for _, sample := range vector {
		labels := sample.Metric
		drive := string(labels[driveLabel])
		if !matchesVMI(labels, vmi) || drive == "" {
			continue
		}
		if !validMetricSample(sample) {
			return fmt.Errorf("invalid monitoring rate %s for disk %s", metric, drive)
		}
		if rates[drive] == nil {
			rates[drive] = diskRates{}
		}
		if _, duplicate := rates[drive][metric]; duplicate {
			return fmt.Errorf("ambiguous monitoring rate %s for disk %s", metric, drive)
		}
		rates[drive][metric] = float64(sample.Value)
	}
	return nil
}

func diskPerformance(rates diskRates) []string {
	readOps, hasReadOps := rates[IOPSRead]
	writeOps, hasWriteOps := rates[IOPSWrite]
	readBytes, hasReadBytes := rates[ReadTrafficBytes]
	writeBytes, hasWriteBytes := rates[WriteTrafficBytes]
	readSeconds, hasReadSeconds := rates[ReadTimesSeconds]
	writeSeconds, hasWriteSeconds := rates[WriteTimesSeconds]
	flushOps, hasFlushOps := rates[FlushRequests]
	flushSeconds, hasFlushSeconds := rates[FlushTimesSeconds]

	totalOps, hasTotalOps := readOps+writeOps, hasReadOps && hasWriteOps
	return []string{
		formatDomainRate(readOps, hasReadOps),
		formatDomainRate(writeOps, hasWriteOps),
		formatDomainRate(totalOps, hasTotalOps),
		formatDomainRate(readBytes/bytesPerMiB, hasReadBytes),
		formatDomainRate(writeBytes/bytesPerMiB, hasWriteBytes),
		formatDomainLatency(readOps, readSeconds, hasReadOps && hasReadSeconds),
		formatDomainLatency(writeOps, writeSeconds, hasWriteOps && hasWriteSeconds),
		formatDomainLatency(totalOps, readSeconds+writeSeconds, hasTotalOps && hasReadSeconds && hasWriteSeconds),
		formatDomainRate(flushOps, hasFlushOps),
		formatDomainLatency(flushOps, flushSeconds, hasFlushOps && hasFlushSeconds),
	}
}

func formatDomainRate(rate float64, available bool) string {
	if !available {
		return Unavailable
	}
	return fmt.Sprintf(metricValueFormat, rate)
}

func formatDomainLatency(operations, seconds float64, available bool) string {
	switch {
	case !available:
		return Unavailable
	case operations <= 0:
		return noCompletedOperations
	default:
		return fmt.Sprintf(metricValueFormat, seconds/operations*millisecondsPerSecond)
	}
}
