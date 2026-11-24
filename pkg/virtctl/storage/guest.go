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
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/prometheus/common/model"
	"github.com/spf13/cobra"
	v1 "kubevirt.io/api/core/v1"
)

const (
	FilesystemCapacity = "kubevirt_vmi_filesystem_capacity_bytes"
	FilesystemUsed     = "kubevirt_vmi_filesystem_used_bytes"
	guestDeviceLabel   = "disk_name"
	guestMountLabel    = "mount_point"
	bytesPerGiB        = 1024 * 1024 * 1024
)

type guestFilesystem struct {
	device, mount, capacity, used string
}

func (s *storageCommand) printGuestFilesystems(cmd *cobra.Command, vmi *v1.VirtualMachineInstance) error {
	if vmi == nil || !vmi.IsRunning() || vmi.Status.NodeName == "" {
		cmd.Println("\nGuest filesystems: unavailable (VMI is not running on a node).")
		return nil
	}
	at := time.Now()
	filesystems, err := s.queryGuest(cmd.Context(), vmi, at)
	if err != nil {
		if cmd.Context().Err() != nil {
			return cmd.Context().Err()
		}
		cmd.Println("\nGuest filesystems: unavailable")
		cmd.PrintErrf("Warning: Guest filesystems unavailable: %v\n", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), monitoringTimeout)
	defer cancel()
	info, err := s.client.VirtualMachineInstance(vmi.Namespace).FilesystemList(ctx, vmi.Name)
	if err != nil {
		if cmd.Context().Err() != nil {
			return cmd.Context().Err()
		}
		info = v1.VirtualMachineInstanceFileSystemList{}
		cmd.PrintErrf("Warning: cannot map guest filesystems to disks: %v\n", err)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "\nGuest filesystems (Prometheus, evaluated at %s):\n", at.UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "DEVICE\tMOUNT POINT\tCAPACITY GiB\tUSED GiB\tVM DISK")
	ordered := slices.SortedFunc(maps.Values(filesystems), func(a, b guestFilesystem) int {
		if a.mount == b.mount {
			return strings.Compare(a.device, b.device)
		}
		return strings.Compare(a.mount, b.mount)
	})
	unmapped := false
	for _, fs := range ordered {
		disk := guestDiskNames(vmi, info, fs.device, fs.mount)
		if disk == Unresolved {
			disk, unmapped = "-", true
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", fs.device, fs.mount, fs.capacity, fs.used, disk)
	}
	if unmapped {
		fmt.Fprintln(w, `
VM DISK: "-" means the association is unavailable.`)
	}
	return w.Flush()
}

func (s *storageCommand) queryGuest(ctx context.Context, vmi *v1.VirtualMachineInstance, at time.Time) (map[string]guestFilesystem, error) {
	if state := vmi.Status.MigrationState; state != nil && !state.Completed {
		return nil, fmt.Errorf("VMI migration is in progress; retry after completion")
	}
	ctx, cancel := context.WithTimeout(ctx, monitoringTimeout)
	defer cancel()
	api, err := s.monitoringAPI()
	if err != nil {
		return nil, err
	}
	rows := map[string]guestFilesystem{}
	cutoff := at.Add(-min(monitoringMaxAge, domainWindow(vmi, at))).Unix()
	for _, metric := range []string{FilesystemCapacity, FilesystemUsed} {
		selector := fmt.Sprintf("%s{namespace=%q,name=%q,node=%q}", metric, vmi.Namespace, vmi.Name, vmi.Status.NodeName)
		query := fmt.Sprintf("%s and (timestamp(%s) >= %d)", selector, selector, cutoff)
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
		for _, sample := range vector {
			labels := sample.Metric
			device, mount := string(labels[guestDeviceLabel]), string(labels[guestMountLabel])
			if labels[namespaceLabel] != model.LabelValue(vmi.Namespace) || labels[nameLabel] != model.LabelValue(vmi.Name) || labels[nodeLabel] != model.LabelValue(vmi.Status.NodeName) || mount == "" {
				continue
			}
			value := float64(sample.Value)
			if sample.Histogram != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return nil, fmt.Errorf("invalid filesystem value %s for mount %s", metric, mount)
			}
			key := fmt.Sprintf("%q:%q", device, mount)
			row, found := rows[key]
			if !found {
				row = guestFilesystem{present(device, NotReported), mount, Unavailable, Unavailable}
			}
			field := &row.capacity
			if metric == FilesystemUsed {
				field = &row.used
			}
			if *field != Unavailable {
				return nil, fmt.Errorf("ambiguous filesystem value %s for mount %s", metric, mount)
			}
			*field = fmt.Sprintf(metricValueFormat, value/bytesPerGiB)
			rows[key] = row
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no recent filesystem metrics for VMI %s/%s; check guest-agent filesystem reporting", vmi.Namespace, vmi.Name)
	}
	return rows, nil
}

// Guest filesystem serials identify backing disks even when device names differ.
func guestDiskNames(vmi *v1.VirtualMachineInstance, info v1.VirtualMachineInstanceFileSystemList, device, mount string) string {
	serials := map[string]string{}
	for _, disk := range vmi.Spec.Domain.Devices.Disks {
		if disk.Serial != "" {
			if _, duplicate := serials[disk.Serial]; duplicate {
				serials[disk.Serial] = Unresolved
			} else {
				serials[disk.Serial] = disk.Name
			}
		}
	}
	var names []string
	found := false
	for _, fs := range info.Items {
		if fs.DiskName != device || fs.MountPoint != mount {
			continue
		}
		if found || len(fs.Disk) == 0 {
			return Unresolved
		}
		found = true
		for _, disk := range fs.Disk {
			name := serials[disk.Serial]
			if name == "" || name == Unresolved {
				return Unresolved
			}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return Unresolved
	}
	sort.Strings(names)
	return strings.Join(slices.Compact(names), ",")
}
