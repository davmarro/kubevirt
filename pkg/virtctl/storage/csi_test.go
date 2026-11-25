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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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

var _ = Describe("PVC warnings", func() {
	BeforeEach(mockStorageClient)

	DescribeTable("shows only the latest warning for each resolved PVC",
		func(change func(*fixture, *corev1.Event, *corev1.Event), expected []string, lists int, stderrWarning string) {
			f := newFixture()
			at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			older := &corev1.Event{
				ObjectMeta: metav1.ObjectMeta{Name: "z-older", Namespace: testNamespace, CreationTimestamp: metav1.NewTime(at)},
				InvolvedObject: corev1.ObjectReference{
					Kind: "PersistentVolumeClaim", Namespace: f.pvc.Namespace, Name: f.pvc.Name, UID: f.pvc.UID,
				},
				Type: corev1.EventTypeWarning, Reason: "ProvisioningFailed", Message: "earlier error",
				LastTimestamp: metav1.NewTime(at),
			}
			newer := older.DeepCopy()
			newer.Name = "a-newer"
			newer.Message = "last error\nretrying"
			newer.LastTimestamp = metav1.NewTime(at.Add(time.Minute))
			f.coreExtras = append(f.coreExtras, older, newer)
			if change != nil {
				change(f, older, newer)
			}

			out, stderr, err := f.run("inspect", "demo")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("DISK"))
			Expect(f.eventLists).To(Equal(lists))
			_, section, found := strings.Cut(out, "Volume warnings:\n")
			if len(expected) == 0 {
				Expect(found).To(BeFalse())
			} else {
				Expect(found).To(BeTrue())
				warnings, _, _ := strings.Cut(section, "\n\n")
				Expect(normalizedReportLines(warnings)).To(Equal(append([]string{"PVC REASON MESSAGE"}, expected...)))
			}
			if stderrWarning == "" {
				Expect(stderr).To(BeEmpty())
			} else {
				Expect(stderr).To(ContainSubstring(stderrWarning))
			}
		},
		Entry("orders warnings by their last occurrence", nil,
			[]string{`claim-a ProvisioningFailed "last error\nretrying"`}, 1, ""),
		Entry("uses the last observation of a repeated event", func(_ *fixture, older, newer *corev1.Event) {
			older.Series = &corev1.EventSeries{LastObservedTime: metav1.NewMicroTime(newer.LastTimestamp.Add(time.Minute))}
		}, []string{`claim-a ProvisioningFailed "earlier error"`}, 1, ""),
		Entry("uses the observation time of a singleton event", func(_ *fixture, older, newer *corev1.Event) {
			older.EventTime = metav1.NewMicroTime(newer.LastTimestamp.Add(time.Minute))
			older.LastTimestamp = metav1.Time{}
		}, []string{`claim-a ProvisioningFailed "earlier error"`}, 1, ""),
		Entry("uses the first observation of a legacy event without a last timestamp", func(_ *fixture, older, newer *corev1.Event) {
			older.FirstTimestamp = metav1.NewTime(newer.LastTimestamp.Add(time.Minute))
			older.LastTimestamp = metav1.Time{}
		}, []string{`claim-a ProvisioningFailed "earlier error"`}, 1, ""),
		Entry("falls back to creation time when observation times are absent", func(_ *fixture, older, newer *corev1.Event) {
			older.CreationTimestamp = metav1.NewTime(newer.LastTimestamp.Add(time.Minute))
			older.LastTimestamp = metav1.Time{}
		}, []string{`claim-a ProvisioningFailed "earlier error"`}, 1, ""),
		Entry("skips events for an earlier PVC of the same name", func(_ *fixture, older, newer *corev1.Event) {
			older.InvolvedObject.UID, newer.InvolvedObject.UID = "old-claim-uid", "old-claim-uid"
		}, nil, 1, ""),
		Entry("skips normal events and unrelated resource kinds", func(_ *fixture, older, newer *corev1.Event) {
			older.Type = corev1.EventTypeNormal
			newer.InvolvedObject.Kind = "Pod"
		}, nil, 1, ""),
		Entry("omits the section when no warnings exist", func(f *fixture, _, _ *corev1.Event) {
			f.coreExtras = nil
		}, nil, 1, ""),
		Entry("shows a provisioning warning for an unbound PVC of a stopped VM", func(f *fixture, _, _ *corev1.Event) {
			f.vmi = nil
			f.pvc.Spec.VolumeName = ""
			f.pvc.Status.Phase = corev1.ClaimPending
		}, []string{`claim-a ProvisioningFailed "last error\nretrying"`}, 1, ""),
		Entry("deduplicates disks backed by the same claim", func(f *fixture, _, _ *corev1.Event) {
			volume := f.vmi.Spec.Volumes[0]
			volume.Name = "other-disk"
			f.vmi.Spec.Volumes = append(f.vmi.Spec.Volumes, volume)
			f.vmi.Spec.Domain.Devices.Disks = append(f.vmi.Spec.Domain.Devices.Disks, v1.Disk{Name: volume.Name})
		}, []string{`claim-a ProvisioningFailed "last error\nretrying"`}, 1, ""),
		Entry("shows a warning for each affected claim with one API call", func(f *fixture, _, newer *corev1.Event) {
			other := f.pvc.DeepCopy()
			other.Name, other.UID, other.Spec.VolumeName = "claim-b", "other-uid", ""
			event := newer.DeepCopy()
			event.Name, event.Message = "other-event", "other error"
			event.InvolvedObject.Name, event.InvolvedObject.UID = other.Name, other.UID
			f.coreExtras = append(f.coreExtras, other, event)
			volume := f.vmi.Spec.Volumes[0].DeepCopy()
			volume.Name, volume.PersistentVolumeClaim.ClaimName = "other-disk", other.Name
			f.vmi.Spec.Volumes = append(f.vmi.Spec.Volumes, *volume)
			f.vmi.Spec.Domain.Devices.Disks = append(f.vmi.Spec.Domain.Devices.Disks, v1.Disk{Name: volume.Name})
		}, []string{`claim-a ProvisioningFailed "last error\nretrying"`, `claim-b ProvisioningFailed "other error"`}, 1, ""),
		Entry("skips the lookup without a resolved claim", func(f *fixture, _, _ *corev1.Event) {
			f.pvc = nil
		}, nil, 0, "cannot read PVC"),
		Entry("preserves the report when listing events is forbidden", func(f *fixture, _, _ *corev1.Event) {
			f.fail = "events"
		}, nil, 1, "cannot list PVC events"),
	)
})
