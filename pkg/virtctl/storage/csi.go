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

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	storagev1 "k8s.io/client-go/kubernetes/typed/storage/v1"
	v1 "kubevirt.io/api/core/v1"
)

type csiLevel struct {
	client storagev1.StorageV1Interface
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
