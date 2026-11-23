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
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/virtctl/clientconfig"
	"kubevirt.io/kubevirt/pkg/virtctl/templates"
)

const PVC = "PVC"
const DataVolume = "DataVolume"
const EphemeralPVC = "ephemeral PVC"
const ContainerDisk = "containerDisk"
const CloudInit = "cloud-init"
const EmptyDisk = "emptyDisk"
const HostDisk = "hostDisk"
const Unresolved = "unresolved"
const Unavailable = "unavailable"
const NotCSI = "not CSI"
const Unknown = "unknown"
const NotApplicable = "not applicable"
const VMTemplate = "VM template (no current VMI)"
const NotPresent = "not present"
const NotScheduled = "not scheduled"
const VMTemplateFinal = "VM template (VMI is final)"
const CurrentVMI = "current VMI"
const NotAvailable = "not available"
const NotReported = "not reported"
const VolumeMissing = "volume missing"

type storageCommand struct {
	client        kubecli.KubevirtClient
	namespace     string
	prometheusURL string
	prometheusCA  string
}

type diskInfo struct {
	name, source, pvc, pv, driver, pvcStatus string
}

// NewCommand creates the storage inspection commands.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "Inspect VM storage resources (PoC).",
	}
	cmd.SetUsageTemplate(templates.UsageTemplate())
	storageCommand := newStorageCommand()
	inspect := &cobra.Command{
		Use:   "inspect [VM]",
		Short: "List namespace VMs or inspect the disks of one VM.",
		Example: `  {{ProgramName}} storage inspect -n my-namespace
  {{ProgramName}} storage inspect my-vm -n my-namespace`,
		Args: cobra.MaximumNArgs(1),
		RunE: storageCommand.run,
	}
	inspect.SetUsageTemplate(templates.UsageTemplate())
	inspect.Flags().StringVar(&storageCommand.prometheusURL, prometheusURLFlag, "", "Prometheus-compatible base URL (required for Domain metrics).")
	inspect.Flags().StringVar(&storageCommand.prometheusCA, prometheusCAFlag, "", "CA certificate file for the monitoring endpoint (default: system trust).")
	cmd.AddCommand(inspect)
	return cmd
}

func newStorageCommand() *storageCommand {
	return &storageCommand{}
}

func (s *storageCommand) run(cmd *cobra.Command, args []string) error {
	var err error
	s.client, s.namespace, _, err = clientconfig.ClientAndNamespaceFromContext(cmd.Context())
	if err != nil {
		return fmt.Errorf("cannot obtain KubeVirt client: %v", err)
	}
	if len(args) == 0 {
		return s.listVMs(cmd)
	}
	virtualMachineName := args[0]
	return s.inspectVM(cmd, virtualMachineName)
}

func (s *storageCommand) listVMs(cmd *cobra.Command) error {
	options := metav1.ListOptions{}
	var items []v1.VirtualMachine
	for {
		vms, err := s.client.GeneratedKubeVirtClient().KubevirtV1().VirtualMachines(s.namespace).List(cmd.Context(), options)
		if err != nil {
			return fmt.Errorf("cannot list VMs in namespace %s: %w", s.namespace, err)
		}
		items = append(items, vms.Items...)
		if vms.Continue == "" {
			break
		}
		options.Continue = vms.Continue
	}

	if len(items) == 0 {
		cmd.Printf("No virtual machines found in namespace %s.\n", s.namespace)
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Namespace: %s\n\nNAME\tSTATUS\n", s.namespace)
	for _, vm := range items {
		fmt.Fprintf(w, "%s\t%s\n", vm.Name, present(string(vm.Status.PrintableStatus), Unknown))
	}
	return w.Flush()
}

func (s *storageCommand) inspectVM(cmd *cobra.Command, name string) error {
	ctx := cmd.Context()
	api := s.client.GeneratedKubeVirtClient().KubevirtV1()
	vm, err := api.VirtualMachines(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot read VM %s/%s: %w", s.namespace, name, err)
	}
	vmi, err := api.VirtualMachineInstances(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("cannot read VMI %s/%s: %w", s.namespace, name, err)
	}
	if apierrors.IsNotFound(err) {
		vmi = nil
	}
	if vmi != nil && !metav1.IsControlledBy(vmi, vm) {
		return fmt.Errorf("VMI %s/%s is not controlled by the selected VM", s.namespace, name)
	}

	var spec *v1.VirtualMachineInstanceSpec
	configuration, phase, nodeName := VMTemplate, NotPresent, NotScheduled
	if vm.Spec.Template != nil {
		spec = &vm.Spec.Template.Spec
	}
	if vmi != nil {
		phase = present(string(vmi.Status.Phase), Unknown)
		if vmi.IsFinal() {
			configuration = VMTemplateFinal
		} else {
			spec = &vmi.Spec
			configuration = CurrentVMI
			nodeName = present(vmi.Status.NodeName, NotScheduled)
		}
	}
	if spec == nil {
		return fmt.Errorf("VM %s/%s has no instance template or current VMI specification", s.namespace, name)
	}

	osImage, kernel := NotAvailable, NotAvailable
	if vmi != nil && !vmi.IsFinal() && vmi.Status.NodeName != "" {
		node, err := s.client.CoreV1().Nodes().Get(ctx, vmi.Status.NodeName, metav1.GetOptions{})
		if err != nil {
			cmd.PrintErrf("Warning: cannot read node %s: %v\n", vmi.Status.NodeName, err)
		} else {
			osImage = present(node.Status.NodeInfo.OSImage, NotReported)
			kernel = present(node.Status.NodeInfo.KernelVersion, NotReported)
		}
	}

	volumes := make(map[string]v1.Volume, len(spec.Volumes))
	for _, volume := range spec.Volumes {
		volumes[volume.Name] = volume
	}
	claims := map[string]string{}
	if vmi != nil && !vmi.IsFinal() {
		for _, status := range vmi.Status.VolumeStatus {
			if info := status.PersistentVolumeClaimInfo; info != nil {
				claims[status.Name] = info.ClaimName
			}
		}
	}
	rows := make([]diskInfo, 0, len(spec.Domain.Devices.Disks))
	for _, disk := range spec.Domain.Devices.Disks {
		volume, found := volumes[disk.Name]
		if !found {
			rows = append(rows, diskInfo{disk.Name, VolumeMissing, "-", "-", "-", Unresolved})
			continue
		}
		rows = append(rows, s.inspectDisk(cmd, volume, claims[disk.Name]))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "VM: %s/%s\nVMI phase: %s\nDisk configuration: %s\nNode: %s\nNode OS: %s\nKernel: %s\n\n", s.namespace, name, phase, configuration, nodeName, osImage, kernel)
	if len(rows) == 0 {
		fmt.Fprintln(w, "No disks configured.")
	} else {
		fmt.Fprintln(w, "DISK\tSOURCE\tPVC\tPV\tCSI DRIVER\tPVC STATUS")
		for _, row := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", row.name, row.source, row.pvc, row.pv, row.driver, row.pvcStatus)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return s.printDomainPerformance(cmd, vmi, rows)
}

func (s *storageCommand) inspectDisk(cmd *cobra.Command, volume v1.Volume, currentClaim string) diskInfo {
	row := diskInfo{name: volume.Name, source: "non-PVC", pvc: "-", pv: "-", driver: "-", pvcStatus: NotApplicable}
	var claimName string
	switch {
	case volume.PersistentVolumeClaim != nil:
		row.source, claimName = PVC, volume.PersistentVolumeClaim.ClaimName
	case volume.DataVolume != nil:
		row.source = DataVolume
		if currentClaim == "" {
			dv, err := s.client.CdiClient().CdiV1beta1().DataVolumes(s.namespace).Get(cmd.Context(), volume.DataVolume.Name, metav1.GetOptions{})
			if err != nil {
				row.pvcStatus = Unresolved
				cmd.PrintErrf("Warning: cannot resolve DataVolume %s/%s: %v\n", s.namespace, volume.DataVolume.Name, err)
				return row
			}
			claimName = present(dv.Status.ClaimName, dv.Name)
		}
	case volume.Ephemeral != nil && volume.Ephemeral.PersistentVolumeClaim != nil:
		row.source, claimName = EphemeralPVC, volume.Ephemeral.PersistentVolumeClaim.ClaimName
	case volume.ContainerDisk != nil:
		row.source = ContainerDisk
	case volume.CloudInitNoCloud != nil || volume.CloudInitConfigDrive != nil:
		row.source = CloudInit
	case volume.EmptyDisk != nil:
		row.source = EmptyDisk
	case volume.HostDisk != nil:
		row.source = HostDisk
	}
	if row.source != PVC && row.source != DataVolume && row.source != EphemeralPVC {
		return row
	}
	// The VMI status records the current claim, including after volume migration.
	claimName = present(currentClaim, claimName)
	if claimName == "" {
		row.pvcStatus = Unresolved
		return row
	}
	row.pvc = claimName
	pvc, err := s.client.CoreV1().PersistentVolumeClaims(s.namespace).Get(cmd.Context(), claimName, metav1.GetOptions{})
	if err != nil {
		row.pvcStatus = Unavailable
		cmd.PrintErrf("Warning: cannot read PVC %s/%s: %v\n", s.namespace, claimName, err)
		return row
	}
	row.pvcStatus = present(string(pvc.Status.Phase), Unknown)
	if pvc.Spec.VolumeName == "" {
		return row
	}
	row.pv = pvc.Spec.VolumeName
	pv, err := s.client.CoreV1().PersistentVolumes().Get(cmd.Context(), row.pv, metav1.GetOptions{})
	if err != nil {
		row.driver = Unavailable
		cmd.PrintErrf("Warning: cannot read PV %s: %v\n", row.pv, err)
		return row
	}
	if ref := pv.Spec.ClaimRef; ref != nil && (ref.Namespace != s.namespace || ref.Name != pvc.Name || (ref.UID != "" && ref.UID != pvc.UID)) {
		row.driver = Unresolved
		cmd.PrintErrf("Warning: PV %s claim reference does not match PVC %s/%s\n", row.pv, s.namespace, pvc.Name)
		return row
	}
	row.driver = NotCSI
	if pv.Spec.CSI != nil {
		row.driver = present(pv.Spec.CSI.Driver, "not reported")
	}
	return row
}

func present(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
