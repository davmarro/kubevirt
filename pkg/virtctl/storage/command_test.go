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
	"bytes"
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	"github.com/spf13/cobra"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	v1 "kubevirt.io/api/core/v1"
	cdifake "kubevirt.io/client-go/containerizeddataimporter/fake"
	"kubevirt.io/client-go/kubecli"
	kvfake "kubevirt.io/client-go/kubevirt/fake"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	"net/http"
	"net/http/httptest"
	"time"

	"kubevirt.io/kubevirt/pkg/virtctl/clientconfig"
)

const testNamespace = "storage-test"

type fixture struct {
	vm              *v1.VirtualMachine
	vmi             *v1.VirtualMachineInstance
	pvc             *corev1.PersistentVolumeClaim
	pv              *corev1.PersistentVolume
	node            *corev1.Node
	dv              *cdiv1.DataVolume
	extraVM         *v1.VirtualMachine
	fail            string
	pages           []*v1.VirtualMachineList
	rates           domainRates
	monitorStatus   int
	monitorBody     string
	filesystems     map[string]model.Vector
	filesystemInfo  v1.VirtualMachineInstanceFileSystemList
	filesystemError error
	attachments     []*storagev1.VolumeAttachment
	attachmentLists int
	coreExtras      []runtime.Object
	eventLists      int
}

func newFixture() *fixture {
	spec := v1.VirtualMachineInstanceSpec{
		Domain: v1.DomainSpec{Devices: v1.Devices{Disks: []v1.Disk{{Name: "data", Serial: "serial-data"}}}},
		Volumes: []v1.Volume{{Name: "data", VolumeSource: v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
			PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: "claim-a"},
		}}}},
	}
	vm := &v1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: testNamespace, UID: "vm-uid"},
		Spec:       v1.VirtualMachineSpec{Template: &v1.VirtualMachineInstanceTemplateSpec{Spec: spec}},
		Status:     v1.VirtualMachineStatus{PrintableStatus: v1.VirtualMachineStatusRunning},
	}
	vmi := &v1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: testNamespace, UID: "vmi-uid", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(vm, v1.VirtualMachineGroupVersionKind)}},
		Spec:       *spec.DeepCopy(),
		Status:     v1.VirtualMachineInstanceStatus{Phase: v1.Running, NodeName: "worker-a"},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-a", Namespace: testNamespace, UID: "claim-uid"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-a"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-a"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "example.csi.test", VolumeHandle: "opaque-volume-id",
			}},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "Test Linux", KernelVersion: "6.12.0"}},
	}
	return &fixture{vm: vm, vmi: vmi, pvc: pvc, pv: pv, node: node, filesystems: testFilesystems(), filesystemInfo: testFilesystemInfo(), rates: domainRates{"data": diskRates{IOPSRead: 0, IOPSWrite: 0, ReadTrafficBytes: 0, WriteTrafficBytes: 0, ReadTimesSeconds: 0, WriteTimesSeconds: 0, FlushRequests: 0, FlushTimesSeconds: 0}}}
}

func (f *fixture) run(args ...string) (string, string, error) {
	var virtObjects, coreObjects, cdiObjects []runtime.Object
	if f.vm != nil {
		virtObjects = append(virtObjects, f.vm)
	}
	if f.vmi != nil {
		virtObjects = append(virtObjects, f.vmi)
	}
	if f.extraVM != nil {
		virtObjects = append(virtObjects, f.extraVM)
	}
	if f.pvc != nil {
		coreObjects = append(coreObjects, f.pvc)
	}
	if f.pv != nil {
		coreObjects = append(coreObjects, f.pv)
	}
	if f.node != nil {
		coreObjects = append(coreObjects, f.node)
	}
	if f.dv != nil {
		cdiObjects = append(cdiObjects, f.dv)
	}
	virt := kvfake.NewSimpleClientset(virtObjects...)
	for _, attachment := range f.attachments {
		coreObjects = append(coreObjects, attachment)
	}
	core := k8sfake.NewSimpleClientset(append(coreObjects, f.coreExtras...)...)
	cdi := cdifake.NewSimpleClientset(cdiObjects...)
	queries := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer GinkgoRecover()
		queries++
		Expect(r.URL.Path).To(Equal("/api/v1/query"))
		Expect(r.Header.Get("Authorization")).To(BeEmpty())
		if f.monitorStatus != 0 {
			w.WriteHeader(f.monitorStatus)
		}
		if f.monitorBody != "" {
			fmt.Fprint(w, f.monitorBody)
			return
		}
		if strings.Contains(r.FormValue("query"), "kubevirt_vmi_filesystem_") {
			writeFilesystems(w, r, f.filesystems)
			return
		}
		writeRates(w, r, f.rates)
	}))
	defer server.Close()

	if f.fail != "" {
		fail := func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: f.fail}, "demo", fmt.Errorf("test access denied"))
		}
		virt.PrependReactor("*", f.fail, fail)
		core.PrependReactor("*", f.fail, fail)
		cdi.PrependReactor("*", f.fail, fail)
	}
	if len(f.pages) > 0 {
		page := 0
		virt.PrependReactor("list", "virtualmachines", func(action ktesting.Action) (bool, runtime.Object, error) {
			Expect(page).To(BeNumerically("<", len(f.pages)))
			expectedToken := ""
			if page > 0 {
				expectedToken = f.pages[page-1].Continue
			}
			Expect(action.(ktesting.ListActionImpl).GetListOptions().Continue).To(Equal(expectedToken))
			result := f.pages[page].DeepCopy()
			page++
			return true, result, nil
		})
		DeferCleanup(func() { Expect(page).To(Equal(len(f.pages))) })
	}
	client := kubecli.NewMockKubevirtClient(gomock.NewController(GinkgoT()))
	client.EXPECT().GeneratedKubeVirtClient().Return(virt).AnyTimes()
	vmiClient := kubecli.NewMockVirtualMachineInstanceInterface(gomock.NewController(GinkgoT()))
	client.EXPECT().VirtualMachineInstance(testNamespace).Return(vmiClient).AnyTimes()
	vmiClient.EXPECT().FilesystemList(gomock.Any(), "demo").Return(f.filesystemInfo, f.filesystemError).AnyTimes()
	client.EXPECT().CoreV1().Return(core.CoreV1()).AnyTimes()
	client.EXPECT().StorageV1().Return(core.StorageV1()).AnyTimes()
	client.EXPECT().CdiClient().Return(cdi).AnyTimes()
	client.EXPECT().Config().Return(testMonitoringConfig()).MaxTimes(1)
	kubecli.MockKubevirtClientInstance = client
	cmd := NewCommand()
	cmd.SetContext(clientconfig.NewContext(context.Background(), clientcmd.NewNonInteractiveClientConfig(
		clientcmdapi.Config{}, "", &clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: testNamespace}}, nil)))
	var out, warnings bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&warnings)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(append(args, "--"+prometheusURLFlag+"="+server.URL))
	err := cmd.Execute()
	f.attachmentLists, f.eventLists = 0, 0
	for _, action := range core.Actions() {
		switch action.GetResource().Resource {
		case "volumeattachments":
			Expect(action.GetVerb()).To(Equal("list"))
			Expect(action.GetNamespace()).To(BeEmpty())
			f.attachmentLists++
		case "events":
			Expect(action.GetVerb()).To(Equal("list"))
			Expect(action.GetNamespace()).To(Equal(testNamespace))
			f.eventLists++
		}
	}
	for _, actions := range [][]ktesting.Action{virt.Actions(), core.Actions(), cdi.Actions()} {
		for _, action := range actions {
			Expect(action.GetVerb()).To(BeElementOf("get", "list"))
			if ns := action.GetNamespace(); ns != "" {
				Expect(ns).To(Equal(testNamespace))
			}
		}
	}
	if len(args) == 1 && args[0] == "inspect" {
		Expect(core.Actions()).To(BeEmpty())
		Expect(cdi.Actions()).To(BeEmpty())
		Expect(queries).To(BeZero())
	}
	return out.String(), warnings.String(), err
}

func normalizedReportLines(report string) []string {
	lines := strings.Split(report, "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	return lines
}

func useDataVolume(f *fixture, name, claim string) {
	f.vmi.Spec.Volumes[0].VolumeSource = v1.VolumeSource{DataVolume: &v1.DataVolumeSource{Name: name}}
	f.dv = &cdiv1.DataVolume{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, Status: cdiv1.DataVolumeStatus{ClaimName: claim}}
}

func mockStorageClient() {
	getClient, client := kubecli.GetKubevirtClientFromClientConfig, kubecli.MockKubevirtClientInstance
	DeferCleanup(func() {
		kubecli.GetKubevirtClientFromClientConfig, kubecli.MockKubevirtClientInstance = getClient, client
	})
	kubecli.GetKubevirtClientFromClientConfig = kubecli.GetMockKubevirtClientFromClientConfig
}

var _ = Describe("Storage inspection", func() {
	BeforeEach(mockStorageClient)

	DescribeTable("resolve disk resources without inventing associations", func(change func(*fixture), expected, warning string) {
		f := newFixture()
		change(f)
		out, stderr, err := f.run("inspect", "demo")
		Expect(err).NotTo(HaveOccurred())
		Expect(normalizedReportLines(out)).To(ContainElement(expected))
		if warning == "" {
			Expect(stderr).NotTo(ContainSubstring("Warning:"))
		} else {
			Expect(stderr).To(ContainSubstring(warning))
		}
	},
		Entry("bound CSI PVC", func(*fixture) {}, "data PVC claim-a pv-a example.csi.test Bound unavailable", ""),
		Entry("unbound PVC", func(f *fixture) { f.pvc.Spec.VolumeName = ""; f.pvc.Status.Phase = corev1.ClaimPending }, "data PVC claim-a - - Pending unavailable", ""),
		Entry("non-CSI PV", func(f *fixture) { f.pv.Spec.CSI = nil }, "data PVC claim-a pv-a not CSI Bound not applicable", ""),
		Entry("missing PVC", func(f *fixture) { f.pvc = nil }, "data PVC claim-a - - unavailable unavailable", "cannot read PVC"),
		Entry("missing PV", func(f *fixture) { f.pv = nil }, "data PVC claim-a pv-a unavailable Bound unavailable", "cannot read PV"),
		Entry("mismatched PV claim reference", func(f *fixture) { f.pv.Spec.ClaimRef.UID = "another-claim" }, "data PVC claim-a pv-a unresolved Bound unavailable", "claim reference does not match"),
		Entry("DataVolume with a different claim name", func(f *fixture) { useDataVolume(f, "import-a", "claim-a") }, "data DataVolume claim-a pv-a example.csi.test Bound unavailable", ""),
		Entry("DataVolume default claim name", func(f *fixture) { useDataVolume(f, "claim-a", "") }, "data DataVolume claim-a pv-a example.csi.test Bound unavailable", ""),
		Entry("missing DataVolume", func(f *fixture) { useDataVolume(f, "import-a", ""); f.dv = nil }, "data DataVolume - - - unresolved unavailable", "cannot resolve DataVolume"),
		Entry("current claim from VMI status", func(f *fixture) {
			f.vmi.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "old-claim"
			f.vmi.Status.VolumeStatus = []v1.VolumeStatus{{Name: "data", PersistentVolumeClaimInfo: &v1.PersistentVolumeClaimInfo{ClaimName: "claim-a"}}}
		}, "data PVC claim-a pv-a example.csi.test Bound unavailable", ""),
		Entry("DataVolume removed after population", func(f *fixture) {
			useDataVolume(f, "import-a", "")
			f.dv = nil
			f.vmi.Status.VolumeStatus = []v1.VolumeStatus{{Name: "data", PersistentVolumeClaimInfo: &v1.PersistentVolumeClaimInfo{ClaimName: "claim-a"}}}
		}, "data DataVolume claim-a pv-a example.csi.test Bound unavailable", ""),
		Entry("ephemeral PVC backing", func(f *fixture) {
			f.vmi.Spec.Volumes[0].VolumeSource = v1.VolumeSource{Ephemeral: &v1.EphemeralVolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "claim-a"}}}
		}, "data ephemeral PVC claim-a pv-a example.csi.test Bound unavailable", ""),
		Entry("container disk", func(f *fixture) {
			f.vmi.Spec.Volumes[0].VolumeSource = v1.VolumeSource{ContainerDisk: &v1.ContainerDiskSource{Image: "example/disk"}}
		}, "data containerDisk - - - not applicable not applicable", ""),
		Entry("cloud-init disk", func(f *fixture) {
			f.vmi.Spec.Volumes[0].VolumeSource = v1.VolumeSource{CloudInitNoCloud: &v1.CloudInitNoCloudSource{UserData: "test"}}
		}, "data cloud-init - - - not applicable not applicable", ""),
		Entry("disk without a matching volume", func(f *fixture) { f.vmi.Spec.Volumes = nil }, "data volume missing - - - unresolved unavailable", ""),
		Entry("denied PVC access", func(f *fixture) { f.fail = "persistentvolumeclaims" }, "data PVC claim-a - - unavailable unavailable", "forbidden"),
		Entry("denied PV access", func(f *fixture) { f.fail = "persistentvolumes" }, "data PVC claim-a pv-a unavailable Bound unavailable", "forbidden"),
		Entry("denied DataVolume access", func(f *fixture) { useDataVolume(f, "import-a", "claim-a"); f.fail = "datavolumes" }, "data DataVolume - - - unresolved unavailable", "forbidden"),
	)

	DescribeTable("show the source of VM and node context", func(change func(*fixture), expected []string, warning string) {
		f := newFixture()
		change(f)
		out, stderr, err := f.run("inspect", "demo")
		Expect(err).NotTo(HaveOccurred())
		for _, text := range expected {
			Expect(out).To(ContainSubstring(text))
		}
		if warning == "" {
			Expect(stderr).NotTo(ContainSubstring("Warning:"))
		} else {
			Expect(stderr).To(ContainSubstring(warning))
		}
	},
		Entry("running VM", func(*fixture) {}, []string{"Disk configuration: current VMI", "VMI phase: Running", "Node: worker-a", "Node OS: Test Linux", "Kernel: 6.12.0"}, ""),
		Entry("VM without a VMI", func(f *fixture) { f.vmi = nil }, []string{"VM template (no current VMI)", "VMI phase: not present", "Node: not scheduled", "Node OS: not available"}, ""),
		Entry("final VMI", func(f *fixture) { f.vmi.Status.Phase = v1.Succeeded }, []string{"VM template (VMI is final)", "VMI phase: Succeeded", "Node: not scheduled"}, ""),
		Entry("unscheduled VMI", func(f *fixture) { f.vmi.Status.Phase = v1.Pending; f.vmi.Status.NodeName = "" }, []string{"Disk configuration: current VMI", "VMI phase: Pending", "Node: not scheduled"}, ""),
		Entry("missing node", func(f *fixture) { f.node = nil }, []string{"Node: worker-a", "Node OS: not available", "Kernel: not available"}, "cannot read node"),
		Entry("denied node access", func(f *fixture) { f.fail = "nodes" }, []string{"Node: worker-a", "Node OS: not available"}, "forbidden"),
		Entry("no configured disks", func(f *fixture) { f.vmi.Spec.Domain.Devices.Disks = nil }, []string{"No disks configured."}, ""),
	)

	DescribeTable("reject unreadable or inconsistent VM identity", func(args []string, change func(*fixture), expected string) {
		f := newFixture()
		change(f)
		out, _, err := f.run(args...)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(expected))
		Expect(out).To(BeEmpty())
	},
		Entry("missing VM", []string{"inspect", "demo"}, func(f *fixture) { f.vm = nil }, "cannot read VM"),
		Entry("denied VM access", []string{"inspect", "demo"}, func(f *fixture) { f.fail = "virtualmachines" }, "cannot read VM"),
		Entry("denied VMI access", []string{"inspect", "demo"}, func(f *fixture) { f.fail = "virtualmachineinstances" }, "cannot read VMI"),
		Entry("VMI owned by another VM", []string{"inspect", "demo"}, func(f *fixture) { f.vmi.OwnerReferences[0].UID = "other-vm" }, "not controlled by the selected VM"),
		Entry("missing VM template", []string{"inspect", "demo"}, func(f *fixture) { f.vm.Spec.Template = nil; f.vmi = nil }, "has no instance template"),
		Entry("denied VM listing", []string{"inspect"}, func(f *fixture) { f.fail = "virtualmachines" }, "cannot list VMs in namespace storage-test"),
		Entry("orphan VMI", []string{"inspect", "demo"}, func(f *fixture) { f.vmi.OwnerReferences = nil }, "not controlled by the selected VM"),
	)

	DescribeTable("list namespace VMs", func(change func(*fixture), expected []string) {
		f := newFixture()
		change(f)
		out, stderr, err := f.run("inspect")
		Expect(err).NotTo(HaveOccurred())
		Expect(stderr).To(BeEmpty())
		for _, text := range expected {
			Expect(out).To(ContainSubstring(text))
		}
		if strings.Contains(out, "alpha") {
			Expect(strings.Index(out, "alpha")).To(BeNumerically("<", strings.Index(out, "demo")))
		}
	},
		Entry("one VM", func(*fixture) {}, []string{"Namespace: storage-test", "NAME", "STATUS", "demo", "Running"}),
		Entry("unknown VM status", func(f *fixture) { f.vm.Status.PrintableStatus = "" }, []string{"demo", "unknown"}),
		Entry("empty namespace", func(f *fixture) { f.vm = nil }, []string{"No virtual machines found in namespace storage-test."}),
		Entry("sorted VMs", func(f *fixture) { f.extraVM = f.vm.DeepCopy(); f.extraVM.Name = "alpha" }, []string{"alpha", "demo"}),
		Entry("paginated VMs", func(f *fixture) {
			other := *f.vm.DeepCopy()
			other.Name = "alpha"
			f.pages = []*v1.VirtualMachineList{{ListMeta: metav1.ListMeta{Continue: "next"}, Items: []v1.VirtualMachine{*f.vm}}, {Items: []v1.VirtualMachine{other}}}
		}, []string{"alpha", "demo"}),
		Entry("empty final page", func(f *fixture) {
			f.pages = []*v1.VirtualMachineList{{ListMeta: metav1.ListMeta{Continue: "next"}, Items: []v1.VirtualMachine{*f.vm}}, {}}
		}, []string{"demo", "Running"}),
	)

	It("uses the current VMI disks and sorts them without listing unrelated volumes", func() {
		f := newFixture()
		f.vm.Spec.Template.Spec.Domain.Devices.Disks = []v1.Disk{{Name: "future-disk"}}
		f.vmi.Spec.Domain.Devices.Disks = append(f.vmi.Spec.Domain.Devices.Disks, v1.Disk{Name: "aaa"})
		f.vmi.Spec.Volumes = append(f.vmi.Spec.Volumes,
			v1.Volume{Name: "aaa", VolumeSource: v1.VolumeSource{EmptyDisk: &v1.EmptyDiskSource{}}},
			v1.Volume{Name: "unused-volume", VolumeSource: v1.VolumeSource{EmptyDisk: &v1.EmptyDiskSource{}}},
		)
		out, _, err := f.run("inspect", "demo")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("future-disk"))
		Expect(out).NotTo(ContainSubstring("unused-volume"))
		Expect(strings.Index(out, "aaa")).To(BeNumerically("<", strings.LastIndex(out, "data")))
	})

	DescribeTable("handle CLI input before creating a client", func(args []string, expectedError string) {
		called := false
		cmd := NewCommand()
		inspect := cmd.Commands()[0]
		run := inspect.RunE
		inspect.RunE = func(cmd *cobra.Command, args []string) error {
			called = true
			return run(cmd, args)
		}
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.Execute()
		if expectedError == "" {
			Expect(err).NotTo(HaveOccurred())
		} else {
			Expect(err).To(MatchError(ContainSubstring(expectedError)))
		}
		Expect(called).To(BeFalse())
	},
		Entry("help", []string{"--help"}, ""),
		Entry("inspect help", []string{"inspect", "--help"}, ""),
		Entry("too many VM arguments", []string{"inspect", "a", "b"}, "accepts at most 1 arg(s)"),
		Entry("unsupported flag", []string{"inspect", "--details"}, "unknown flag: --details"),
	)

	DescribeTable("report client configuration errors", func(withContext bool, args []string, expected string) {
		cmd := NewCommand()
		if withContext {
			cmd.SetContext(clientconfig.NewContext(context.Background(), kubecli.DefaultClientConfig(cmd.Flags())))
			kubecli.GetKubevirtClientFromClientConfig = kubecli.GetInvalidKubevirtClientFromClientConfig
		}
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		Expect(cmd.Execute()).To(MatchError("cannot obtain KubeVirt client: " + expected))
	},
		Entry("listing without context", false, []string{"inspect"}, "unable to get client config from context"),
		Entry("inspection without context", false, []string{"inspect", "demo"}, "unable to get client config from context"),
		Entry("listing with an invalid client", true, []string{"inspect"}, "invalid fake client"),
		Entry("inspection with an invalid client", true, []string{"inspect", "demo"}, "invalid fake client"),
	)
})
