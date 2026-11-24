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
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/common/model"
	v1 "kubevirt.io/api/core/v1"
)

const (
	namespaceLabel    = "namespace"
	nameLabel         = "name"
	nodeLabel         = "node"
	monitoringWindow  = 5 * time.Minute
	monitoringMaxAge  = 2 * time.Minute
	metricValueFormat = "%.2f"
)

func vmiRunningOnNode(vmi *v1.VirtualMachineInstance) bool {
	return vmi != nil && vmi.IsRunning() && vmi.Status.NodeName != ""
}

func checkMetricMigration(vmi *v1.VirtualMachineInstance) error {
	if state := vmi.Status.MigrationState; state != nil && !state.Completed {
		return errors.New("VMI migration is in progress; retry after completion")
	}
	return nil
}

func vmiMetricSelector(vmi *v1.VirtualMachineInstance) string {
	return fmt.Sprintf("%s=%q,%s=%q,%s=%q", namespaceLabel, vmi.Namespace, nameLabel, vmi.Name, nodeLabel, vmi.Status.NodeName)
}

func matchesVMI(labels model.Metric, vmi *v1.VirtualMachineInstance) bool {
	return labels[namespaceLabel] == model.LabelValue(vmi.Namespace) &&
		labels[nameLabel] == model.LabelValue(vmi.Name) &&
		labels[nodeLabel] == model.LabelValue(vmi.Status.NodeName)
}

func validMetricSample(sample *model.Sample) bool {
	value := float64(sample.Value)
	return sample.Histogram == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// Keep the lookback inside the current VMI's known lifetime and running segment.
// Metric labels do not include a VMI UID, so a reused name must not select an older lifetime.
func vmiMetricsWindow(vmi *v1.VirtualMachineInstance, at time.Time) time.Duration {
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
