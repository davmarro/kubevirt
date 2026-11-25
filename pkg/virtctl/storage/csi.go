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
	"fmt"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	storagev1 "k8s.io/client-go/kubernetes/typed/storage/v1"
	v1 "kubevirt.io/api/core/v1"
)

type csiLevel struct {
	client storagev1.StorageV1Interface
	events coreclient.EventInterface
}

func (c csiLevel) readAttachments(cmd *cobra.Command, vmi *v1.VirtualMachineInstance, disks []diskInfo) {
	if vmi == nil || vmi.IsFinal() || vmi.Status.NodeName == "" ||
		!slices.ContainsFunc(disks, func(disk diskInfo) bool { return disk.hasCSIDriver }) {
		return
	}
	attachments, err := c.client.VolumeAttachments().List(cmd.Context(), metav1.ListOptions{})
	if err != nil {
		cmd.PrintErrf("Warning: cannot list CSI volume attachments: %v\n", err)
		return
	}

	type volumeKey struct{ pv, driver string }
	statuses := map[volumeKey]string{}
	for _, attachment := range attachments.Items {
		pvName := attachment.Spec.Source.PersistentVolumeName
		if pvName == nil || attachment.Spec.NodeName != vmi.Status.NodeName || attachment.DeletionTimestamp != nil {
			continue
		}
		key := volumeKey{pv: *pvName, driver: attachment.Spec.Attacher}
		_, duplicate := statuses[key]
		switch {
		case duplicate:
			statuses[key] = Unavailable
		case attachment.Status.Attached:
			statuses[key] = "attached"
		case attachment.Status.AttachError != nil && attachment.Status.AttachError.Message != "":
			statuses[key] = fmt.Sprintf("not attached (last error: %q)", attachment.Status.AttachError.Message)
		default:
			statuses[key] = "not attached"
		}
	}
	for i := range disks {
		disk := &disks[i]
		if disk.hasCSIDriver {
			disk.attachment = present(statuses[volumeKey{pv: disk.pv, driver: disk.driver}], Unavailable)
		}
	}
}

func (c csiLevel) printVolumeWarnings(cmd *cobra.Command, disks []diskInfo) error {
	latest := map[types.UID]*corev1.Event{}
	for _, disk := range disks {
		if disk.pvcUID != "" {
			latest[disk.pvcUID] = nil
		}
	}
	if len(latest) == 0 {
		return nil
	}

	events, err := c.events.List(cmd.Context(), metav1.ListOptions{
		FieldSelector: "involvedObject.kind=PersistentVolumeClaim,type=Warning",
	})
	if err != nil {
		cmd.PrintErrf("Warning: cannot list PVC events: %v\n", err)
		return nil
	}
	for i := range events.Items {
		event := &events.Items[i]
		uid := event.InvolvedObject.UID
		previous, known := latest[uid]
		if !known || event.Type != corev1.EventTypeWarning || event.InvolvedObject.Kind != "PersistentVolumeClaim" {
			continue
		}
		if previous == nil || eventLastSeen(event).After(eventLastSeen(previous)) {
			latest[uid] = event
		}
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	printedHeader := false
	for _, disk := range disks {
		event := latest[disk.pvcUID]
		if event == nil {
			continue
		}
		if !printedHeader {
			fmt.Fprintln(w, "\nVolume warnings:\nPVC\tREASON\tMESSAGE")
			printedHeader = true
		}
		fmt.Fprintf(w, "%s\t%s\t%q\n", disk.pvc, event.Reason, event.Message)
		delete(latest, disk.pvcUID)
	}
	return w.Flush()
}

func eventLastSeen(event *corev1.Event) time.Time {
	switch {
	case event.Series != nil && !event.Series.LastObservedTime.IsZero():
		return event.Series.LastObservedTime.Time
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	case !event.FirstTimestamp.IsZero():
		return event.FirstTimestamp.Time
	default:
		return event.CreationTimestamp.Time
	}
}
