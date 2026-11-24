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
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "kubevirt.io/api/core/v1"
)

func filesystemSample(device, mount string, value float64) *model.Sample {
	sample := rateSample("", value)
	delete(sample.Metric, driveLabel)
	sample.Metric[guestDeviceLabel] = model.LabelValue(device)
	sample.Metric[guestMountLabel] = model.LabelValue(mount)
	return sample
}

func testFilesystemInfo() v1.VirtualMachineInstanceFileSystemList {
	return v1.VirtualMachineInstanceFileSystemList{Items: []v1.VirtualMachineInstanceFileSystem{{
		DiskName: "vdb", MountPoint: "/data", Disk: []v1.VirtualMachineInstanceFileSystemDisk{{Serial: "serial-data", BusType: "virtio"}},
	}}}
}

func testFilesystems() map[string]model.Vector {
	return map[string]model.Vector{
		FilesystemCapacity: {filesystemSample("vdb", "/data", 2*bytesPerGiB)},
		FilesystemUsed:     {filesystemSample("vdb", "/data", .5*bytesPerGiB)},
	}
}

func writeFilesystems(w http.ResponseWriter, r *http.Request, filesystems map[string]model.Vector) {
	query := r.FormValue("query")
	vector := model.Vector{}
	for metric, samples := range filesystems {
		if strings.Contains(query, metric+"{") {
			vector = append(vector, samples...)
		}
	}
	writeVector(w, vector)
}

var _ = Describe("Guest filesystem metrics", func() {
	BeforeEach(mockStorageClient)

	DescribeTable("join filesystem gauges by guest device and mount", func(change func(map[string]model.Vector), expected []guestFilesystem, expectedError string) {
		filesystems := testFilesystems()
		change(filesystems)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			writeFilesystems(w, r, filesystems)
		}))
		defer server.Close()
		level := guestLevel{source: testPrometheusSource(server.URL)}
		rows, err := level.query(context.Background(), newFixture().vmi, time.Now())
		if expectedError != "" {
			Expect(err).To(MatchError(ContainSubstring(expectedError)))
		} else {
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(ConsistOf(expected))
		}
	},
		Entry("capacity and used space", func(map[string]model.Vector) {}, []guestFilesystem{{"vdb", "/data", "2.00", "0.50"}}, ""),
		Entry("zero used space", func(f map[string]model.Vector) { f[FilesystemUsed][0].Value = 0 }, []guestFilesystem{{"vdb", "/data", "2.00", "0.00"}}, ""),
		Entry("capacity missing", func(f map[string]model.Vector) { delete(f, FilesystemCapacity) }, []guestFilesystem{{"vdb", "/data", Unavailable, "0.50"}}, ""),
		Entry("used space missing", func(f map[string]model.Vector) { delete(f, FilesystemUsed) }, []guestFilesystem{{"vdb", "/data", "2.00", Unavailable}}, ""),
		Entry("all gauges missing", func(f map[string]model.Vector) { clear(f) }, nil, "no recent filesystem metrics"),
		Entry("unknown device preserves the filesystem", func(f map[string]model.Vector) {
			for _, samples := range f {
				delete(samples[0].Metric, guestDeviceLabel)
			}
		}, []guestFilesystem{{NotReported, "/data", "2.00", "0.50"}}, ""),
		Entry("missing mount identity", func(f map[string]model.Vector) {
			for _, samples := range f {
				delete(samples[0].Metric, guestMountLabel)
			}
		}, nil, "no recent filesystem metrics"),
		Entry("another namespace", func(f map[string]model.Vector) {
			for _, samples := range f {
				samples[0].Metric[namespaceLabel] = "other"
			}
		}, nil, "no recent filesystem metrics"),
		Entry("another VM", func(f map[string]model.Vector) {
			for _, samples := range f {
				samples[0].Metric[nameLabel] = "other"
			}
		}, nil, "no recent filesystem metrics"),
		Entry("another node", func(f map[string]model.Vector) {
			for _, samples := range f {
				samples[0].Metric[nodeLabel] = "worker-b"
			}
		}, nil, "no recent filesystem metrics"),
		Entry("duplicate series", func(f map[string]model.Vector) {
			f[FilesystemCapacity] = append(f[FilesystemCapacity], filesystemSample("vdb", "/data", 2*bytesPerGiB))
		}, nil, "ambiguous filesystem value"),
		Entry("negative used bytes", func(f map[string]model.Vector) { f[FilesystemUsed][0].Value = -1 }, nil, "invalid filesystem value"),
		Entry("NaN capacity", func(f map[string]model.Vector) { f[FilesystemCapacity][0].Value = model.SampleValue(math.NaN()) }, nil, "invalid filesystem value"),
		Entry("infinite capacity", func(f map[string]model.Vector) { f[FilesystemCapacity][0].Value = model.SampleValue(math.Inf(1)) }, nil, "invalid filesystem value"),
		Entry("histogram instead of gauge", func(f map[string]model.Vector) { f[FilesystemCapacity][0].Histogram = &model.SampleHistogram{} }, nil, "invalid filesystem value"),
		Entry("multiple mounts on one guest device", func(f map[string]model.Vector) {
			f[FilesystemCapacity] = append(f[FilesystemCapacity], filesystemSample("vdb", "/data/logs", bytesPerGiB))
			f[FilesystemUsed] = append(model.Vector{filesystemSample("vdb", "/data/logs", .25*bytesPerGiB)}, f[FilesystemUsed]...)
		}, []guestFilesystem{{"vdb", "/data", "2.00", "0.50"}, {"vdb", "/data/logs", "1.00", "0.25"}}, ""),
		Entry("separators in labels do not merge distinct filesystems", func(f map[string]model.Vector) {
			f[FilesystemCapacity] = model.Vector{
				filesystemSample("/dev/vdb:/mnt", "/data", 2*bytesPerGiB),
				filesystemSample("/dev/vdb", "/mnt:/data", 4*bytesPerGiB),
			}
			f[FilesystemUsed] = model.Vector{
				filesystemSample("/dev/vdb", "/mnt:/data", bytesPerGiB),
				filesystemSample("/dev/vdb:/mnt", "/data", .5*bytesPerGiB),
			}
		}, []guestFilesystem{{"/dev/vdb:/mnt", "/data", "2.00", "0.50"}, {"/dev/vdb", "/mnt:/data", "4.00", "1.00"}}, ""),
		Entry("join differently ordered gauges", func(f map[string]model.Vector) {
			f[FilesystemCapacity] = append(f[FilesystemCapacity], filesystemSample("vda1", "/", 4*bytesPerGiB))
			f[FilesystemUsed] = append(model.Vector{filesystemSample("vda1", "/", bytesPerGiB)}, f[FilesystemUsed]...)
		}, []guestFilesystem{{"vda1", "/", "4.00", "1.00"}, {"vdb", "/data", "2.00", "0.50"}}, ""),
	)

	DescribeTable("query gauges at one timestamp with source freshness and known VMI lifetime", func(age time.Duration) {
		at := time.Now().Truncate(time.Second)
		vmi := newFixture().vmi
		vmi.CreationTimestamp = metav1.NewTime(at.Add(-age))
		queries := []string{}
		times := []string{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			Expect(r.ParseForm()).To(Succeed())
			queries = append(queries, r.Form.Get("query"))
			times = append(times, r.Form.Get("time"))
			writeFilesystems(w, r, testFilesystems())
		}))
		defer server.Close()
		level := guestLevel{source: testPrometheusSource(server.URL)}
		_, err := level.query(context.Background(), vmi, at)
		Expect(err).NotTo(HaveOccurred())
		Expect(queries).To(HaveLen(2))
		Expect(times).To(HaveLen(2))
		for i, metric := range []string{FilesystemCapacity, FilesystemUsed} {
			selector := fmt.Sprintf(`%s{namespace="storage-test",name="demo",node="worker-a"}`, metric)
			Expect(queries[i]).To(Equal(fmt.Sprintf("%s and (timestamp(%s) >= %d)", selector, selector, at.Add(-min(age, monitoringMaxAge)).Unix())))
			Expect(times[i]).To(Equal(times[0]))
			Expect(queries[i]).NotTo(ContainSubstring("rate("))
		}
	}, Entry("existing VMI", time.Hour), Entry("recently created VMI", 30*time.Second))

	DescribeTable("display Guest context independently of Domain disk metrics", func(change func(*fixture), expected, warning string) {
		f := newFixture()
		change(f)
		out, stderr, err := f.run("inspect", "demo")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("VM: storage-test/demo"))
		Expect(out).To(ContainSubstring(expected))
		if warning == "" {
			Expect(stderr).To(BeEmpty())
		} else {
			Expect(stderr).To(ContainSubstring(warning))
		}
		if strings.Contains(out, "Guest filesystems (Prometheus,") {
			Expect(out).To(ContainSubstring("DEVICE"))
			Expect(out).To(ContainSubstring("CAPACITY GiB"))
			Expect(out).To(ContainSubstring("USED GiB"))
			lines := strings.Split(strings.SplitN(out, "Guest filesystems (Prometheus,", 2)[1], "\n")
			Expect(strings.Fields(lines[2])[:4]).To(Equal([]string{"vdb", "/data", "2.00", "0.50"}))
		}
	},
		Entry("available gauges", func(*fixture) {}, "Guest filesystems (Prometheus, evaluated at", ""),
		Entry("missing guest-agent filesystem data", func(f *fixture) { f.filesystems = nil }, "Guest filesystems: unavailable", "no recent filesystem metrics"),
		Entry("Domain metrics missing", func(f *fixture) { f.rates = nil }, "Guest filesystems (Prometheus, evaluated at", "no recent storage rates"),
		Entry("no configured Domain disks", func(f *fixture) { f.vmi.Spec.Domain.Devices.Disks = nil }, "Guest filesystems (Prometheus, evaluated at", ""),
		Entry("no VMI", func(f *fixture) { f.vmi = nil }, "Guest filesystems: unavailable (VMI is not running on a node).", ""),
		Entry("completed VMI", func(f *fixture) { f.vmi.Status.Phase = v1.Succeeded }, "Guest filesystems: unavailable (VMI is not running on a node).", ""),
		Entry("unscheduled VMI", func(f *fixture) { f.vmi.Status.NodeName = "" }, "Guest filesystems: unavailable (VMI is not running on a node).", ""),
		Entry("forbidden monitoring", func(f *fixture) { f.monitorStatus = http.StatusForbidden }, "Guest filesystems: unavailable", "cannot query"),
		Entry("unavailable monitoring", func(f *fixture) { f.monitorStatus = http.StatusBadGateway }, "Guest filesystems: unavailable", "cannot query"),
		Entry("invalid monitoring response", func(f *fixture) { f.monitorBody = "bad{" }, "Guest filesystems: unavailable", "cannot query"),
		Entry("partial monitoring result", func(f *fixture) {
			f.monitorBody = `{"status":"success","warnings":["partial response"],"data":{"resultType":"vector","result":[]}}`
		}, "Guest filesystems: unavailable", "incomplete monitoring result"),
		Entry("unexpected response type", func(f *fixture) {
			f.monitorBody = `{"status":"success","data":{"resultType":"scalar","result":[1,"2"]}}`
		}, "Guest filesystems: unavailable", "expected an instant vector"),
		Entry("migration in progress", func(f *fixture) { f.vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{} }, "Guest filesystems: unavailable", "migration is in progress"),
	)

	DescribeTable("map guest filesystems by explicit disk serials", func(change func(*v1.VirtualMachineInstance, *v1.VirtualMachineInstanceFileSystemList), expected string) {
		vmi, info := newFixture().vmi, testFilesystemInfo()
		change(vmi, &info)
		Expect(guestDiskNames(vmi, info, "vdb", "/data")).To(Equal(expected))
	},
		Entry("matching serial", func(*v1.VirtualMachineInstance, *v1.VirtualMachineInstanceFileSystemList) {}, "data"),
		Entry("guest name differs from target", func(v *v1.VirtualMachineInstance, _ *v1.VirtualMachineInstanceFileSystemList) {
			v.Status.VolumeStatus = []v1.VolumeStatus{{Name: "data", Target: "sda"}}
		}, "data"),
		Entry("missing configured serial", func(v *v1.VirtualMachineInstance, _ *v1.VirtualMachineInstanceFileSystemList) {
			v.Spec.Domain.Devices.Disks[0].Serial = ""
		}, Unresolved),
		Entry("missing guest serial", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			f.Items[0].Disk[0].Serial = ""
		}, Unresolved),
		Entry("unknown serial", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			f.Items[0].Disk[0].Serial = "unknown"
		}, Unresolved),
		Entry("missing backing metadata", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) { f.Items[0].Disk = nil }, Unresolved),
		Entry("missing filesystem metadata", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) { f.Items = nil }, Unresolved),
		Entry("different device", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			f.Items[0].DiskName = "vdc"
		}, Unresolved),
		Entry("different mount", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			f.Items[0].MountPoint = "/other"
		}, Unresolved),
		Entry("ambiguous configured serial", func(v *v1.VirtualMachineInstance, _ *v1.VirtualMachineInstanceFileSystemList) {
			v.Spec.Domain.Devices.Disks = append(v.Spec.Domain.Devices.Disks, v1.Disk{Name: "other", Serial: "serial-data"})
		}, Unresolved),
		Entry("duplicate filesystem metadata", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			f.Items = append(f.Items, f.Items[0])
		}, Unresolved),
		Entry("filesystem spans multiple disks", func(v *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			v.Spec.Domain.Devices.Disks = append(v.Spec.Domain.Devices.Disks, v1.Disk{Name: "aaa", Serial: "serial-other"})
			f.Items[0].Disk = append(f.Items[0].Disk, v1.VirtualMachineInstanceFileSystemDisk{Serial: "serial-other"})
		}, "aaa,data"),
		Entry("partially unresolved multi-disk filesystem", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			f.Items[0].Disk = append(f.Items[0].Disk, v1.VirtualMachineInstanceFileSystemDisk{Serial: "unknown"})
		}, Unresolved),
		Entry("same backing disk listed twice", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			f.Items[0].Disk = append(f.Items[0].Disk, f.Items[0].Disk[0])
		}, "data"),
		Entry("same serial on several filesystem partitions", func(_ *v1.VirtualMachineInstance, f *v1.VirtualMachineInstanceFileSystemList) {
			other := f.Items[0]
			other.DiskName = "vdb1"
			other.MountPoint = "/boot"
			f.Items = append([]v1.VirtualMachineInstanceFileSystem{other}, f.Items...)
		}, "data"),
	)

	DescribeTable("display disk association while preserving filesystem values", func(change func(*fixture), expectedRows []string, warning string, legend bool) {
		f := newFixture()
		change(f)
		out, stderr, err := f.run("inspect", "demo")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("Domain performance (Prometheus, lookback"))
		Expect(out).To(ContainSubstring("Guest filesystems (Prometheus, evaluated at"))
		guest := strings.SplitN(out, "Guest filesystems (Prometheus,", 2)[1]
		var lines []string
		for _, line := range strings.Split(guest, "\n") {
			lines = append(lines, strings.Join(strings.Fields(line), " "))
		}
		Expect(lines).To(ContainElement("DEVICE MOUNT POINT CAPACITY GiB USED GiB VM DISK"))
		Expect(strings.Join(lines, "\n")).To(ContainSubstring(strings.Join(expectedRows, "\n")))
		Expect(guest).NotTo(ContainSubstring(Unresolved))
		count := 0
		if legend {
			count = 1
		}
		Expect(strings.Count(guest, `VM DISK: "-" means the association is unavailable.`)).To(Equal(count))
		if warning == "" {
			Expect(stderr).To(BeEmpty())
		} else {
			Expect(stderr).To(ContainSubstring(warning))
		}
	},
		Entry("known disk without legend", func(*fixture) {}, []string{"vdb /data 2.00 0.50 data"}, "", false),
		Entry("missing serial preserves values", func(f *fixture) { f.vmi.Spec.Domain.Devices.Disks[0].Serial = "" }, []string{"vdb /data 2.00 0.50 -"}, "", true),
		Entry("missing filesystem metadata preserves values", func(f *fixture) { f.filesystemInfo.Items = nil }, []string{"vdb /data 2.00 0.50 -"}, "", true),
		Entry("unavailable metadata preserves values", func(f *fixture) { f.filesystemError = fmt.Errorf("filesystem metadata unavailable") }, []string{"vdb /data 2.00 0.50 -"}, "cannot map guest filesystems", true),
		Entry("forbidden metadata preserves values", func(f *fixture) { f.filesystemError = fmt.Errorf("filesystemlist is forbidden") }, []string{"vdb /data 2.00 0.50 -"}, "filesystemlist is forbidden", true),
		Entry("one legend for multiple missing associations", func(f *fixture) {
			f.filesystemInfo.Items = nil
			f.filesystems[FilesystemCapacity] = append(f.filesystems[FilesystemCapacity], filesystemSample("vda1", "/", 4*bytesPerGiB))
			f.filesystems[FilesystemUsed] = append(f.filesystems[FilesystemUsed], filesystemSample("vda1", "/", bytesPerGiB))
		}, []string{"vda1 / 4.00 1.00 -", "vdb /data 2.00 0.50 -"}, "", true),
		Entry("legend with both known and missing associations", func(f *fixture) {
			f.filesystems[FilesystemCapacity] = append(f.filesystems[FilesystemCapacity], filesystemSample("vdz1", "/", 4*bytesPerGiB))
			f.filesystems[FilesystemUsed] = append(f.filesystems[FilesystemUsed], filesystemSample("vdz1", "/", bytesPerGiB))
		}, []string{"vdz1 / 4.00 1.00 -", "vdb /data 2.00 0.50 data"}, "", true),
		Entry("filesystem spanning known disks without legend", func(f *fixture) {
			f.vmi.Spec.Domain.Devices.Disks = append(f.vmi.Spec.Domain.Devices.Disks, v1.Disk{Name: "aaa", Serial: "serial-other"})
			f.filesystemInfo.Items[0].Disk = append(f.filesystemInfo.Items[0].Disk, v1.VirtualMachineInstanceFileSystemDisk{Serial: "serial-other"})
		}, []string{"vdb /data 2.00 0.50 aaa,data"}, "", false),
	)

	DescribeTable("propagate cancellation", func(beforeQuery bool) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if beforeQuery {
			cancel()
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			cancel()
			writeFilesystems(w, r, testFilesystems())
		}))
		defer server.Close()
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		level := guestLevel{source: testPrometheusSource(server.URL)}
		err := level.print(cmd, newFixture().vmi)
		Expect(err).To(MatchError(context.Canceled))
	}, Entry("before query", true), Entry("during query", false))
})
