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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "kubevirt.io/api/core/v1"
)

var _ = Describe("CSI attachment status", func() {
	BeforeEach(mockStorageClient)

	DescribeTable("report the attachment for the VM disk and node", func(change func(*fixture, *storagev1.VolumeAttachment), expected, warning string, lists int) {
		f := newFixture()
		attachment := &storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: "attachment"},
			Spec: storagev1.VolumeAttachmentSpec{
				Attacher: f.pv.Spec.CSI.Driver,
				NodeName: f.vmi.Status.NodeName,
				Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &f.pv.Name},
			},
			Status: storagev1.VolumeAttachmentStatus{Attached: true},
		}
		f.attachments = []*storagev1.VolumeAttachment{attachment}
		change(f, attachment)

		out, stderr, err := f.run("inspect", "demo")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("CSI ATTACHMENT"))
		Expect(normalizedReportLines(out)).To(ContainElement("data PVC claim-a pv-a " + expected))
		Expect(f.attachmentLists).To(Equal(lists))
		if warning == "" {
			Expect(stderr).To(BeEmpty())
		} else {
			Expect(stderr).To(ContainSubstring(warning))
		}
	},
		Entry("attached", func(*fixture, *storagev1.VolumeAttachment) {}, "example.csi.test Bound attached", "", 1),
		Entry("multiple disks share a single attachment list", func(f *fixture, _ *storagev1.VolumeAttachment) {
			volume := f.vmi.Spec.Volumes[0]
			volume.Name = "other-disk"
			f.vmi.Spec.Volumes = append(f.vmi.Spec.Volumes, volume)
			f.vmi.Spec.Domain.Devices.Disks = append(f.vmi.Spec.Domain.Devices.Disks, v1.Disk{Name: volume.Name})
		}, "example.csi.test Bound attached", "", 1),
		Entry("not attached yet", func(_ *fixture, a *storagev1.VolumeAttachment) {
			a.Status.Attached = false
		}, "example.csi.test Bound not attached", "", 1),
		Entry("last attach error stays on the disk row", func(_ *fixture, a *storagev1.VolumeAttachment) {
			a.Status.Attached = false
			a.Status.AttachError = &storagev1.VolumeError{Message: "access denied\nretrying"}
		}, `example.csi.test Bound not attached (last error: "access denied\nretrying")`, "", 1),
		Entry("attached after a previous error", func(_ *fixture, a *storagev1.VolumeAttachment) {
			a.Status.AttachError = &storagev1.VolumeError{Message: "previous error"}
		}, "example.csi.test Bound attached", "", 1),
		Entry("unrelated PV", func(_ *fixture, a *storagev1.VolumeAttachment) {
			a.Spec.Source.PersistentVolumeName = new("other-pv")
		}, "example.csi.test Bound unavailable", "", 1),
		Entry("unrelated node", func(_ *fixture, a *storagev1.VolumeAttachment) {
			a.Spec.NodeName = "worker-b"
		}, "example.csi.test Bound unavailable", "", 1),
		Entry("unrelated driver", func(_ *fixture, a *storagev1.VolumeAttachment) {
			a.Spec.Attacher = "other.csi.test"
		}, "example.csi.test Bound unavailable", "", 1),
		Entry("attachment without a PV source", func(_ *fixture, a *storagev1.VolumeAttachment) {
			a.Spec.Source.PersistentVolumeName = nil
		}, "example.csi.test Bound unavailable", "", 1),
		Entry("no attachment record", func(f *fixture, _ *storagev1.VolumeAttachment) {
			f.attachments = nil
		}, "example.csi.test Bound unavailable", "", 1),
		Entry("ambiguous attachment records", func(f *fixture, a *storagev1.VolumeAttachment) {
			other := a.DeepCopy()
			other.Name = "duplicate"
			f.attachments = append(f.attachments, other)
		}, "example.csi.test Bound unavailable", "", 1),
		Entry("attachment is being deleted", func(_ *fixture, a *storagev1.VolumeAttachment) {
			now := metav1.Now()
			a.DeletionTimestamp = &now
		}, "example.csi.test Bound unavailable", "", 1),
		Entry("forbidden list preserves the disk row", func(f *fixture, _ *storagev1.VolumeAttachment) {
			f.fail = "volumeattachments"
		}, "example.csi.test Bound unavailable", "cannot list CSI volume attachments", 1),
		Entry("non-CSI volume skips the lookup", func(f *fixture, _ *storagev1.VolumeAttachment) {
			f.pv.Spec.CSI = nil
		}, "not CSI Bound not applicable", "", 0),
		Entry("missing driver name skips the lookup", func(f *fixture, _ *storagev1.VolumeAttachment) {
			f.pv.Spec.CSI.Driver = ""
		}, "not reported Bound unavailable", "", 0),
		Entry("stopped VM skips the lookup", func(f *fixture, _ *storagev1.VolumeAttachment) {
			f.vmi = nil
		}, "example.csi.test Bound unavailable", "", 0),
		Entry("completed VMI skips the lookup", func(f *fixture, _ *storagev1.VolumeAttachment) {
			f.vmi.Status.Phase = v1.Succeeded
		}, "example.csi.test Bound unavailable", "", 0),
		Entry("unscheduled VMI skips the lookup", func(f *fixture, _ *storagev1.VolumeAttachment) {
			f.vmi.Status.NodeName = ""
		}, "example.csi.test Bound unavailable", "", 0),
	)
})
