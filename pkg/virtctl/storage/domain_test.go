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

func rateSample(drive string, value float64) *model.Sample {
	return &model.Sample{Metric: model.Metric{namespaceLabel: testNamespace, nameLabel: "demo", nodeLabel: "worker-a", driveLabel: model.LabelValue(drive)}, Value: model.SampleValue(value), Timestamp: model.Now()}
}

func writeRates(w http.ResponseWriter, r *http.Request, rates domainRates) {
	Expect(r.ParseForm()).To(Succeed())
	query := r.Form.Get("query")
	vector := model.Vector{}
	for _, metric := range []string{IOPSRead, IOPSWrite, ReadTrafficBytes, WriteTrafficBytes, ReadTimesSeconds, WriteTimesSeconds, FlushRequests, FlushTimesSeconds} {
		if !strings.Contains(query, kubevirtVmiStoragePrefix+metric+"{") {
			continue
		}
		for drive, values := range rates {
			if value, exists := values[metric]; exists {
				vector = append(vector, rateSample(drive, value))
			}
		}
	}
	writeVector(w, vector)
}

var _ = Describe("Domain performance from Prometheus", func() {
	BeforeEach(mockStorageClient)

	DescribeTable("format per-disk rates and completed-operation latency", func(rates diskRates, expected []string) {
		Expect(diskPerformance(rates)).To(Equal(expected))
	},
		Entry("missing rates", diskRates{}, []string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable}),
		Entry("active reads and writes", diskRates{IOPSRead: 4, IOPSWrite: 2, ReadTrafficBytes: 2 * bytesPerMiB, WriteTrafficBytes: bytesPerMiB, ReadTimesSeconds: .008, WriteTimesSeconds: .01}, []string{"4.00", "2.00", "6.00", "2.00", "1.00", "2.00", "5.00", "3.00", Unavailable, Unavailable}),
		Entry("idle is zero activity and undefined latency", diskRates{IOPSRead: 0, ReadTrafficBytes: 0, ReadTimesSeconds: 0}, []string{"0.00", Unavailable, Unavailable, "0.00", Unavailable, noCompletedOperations, Unavailable, Unavailable, Unavailable, Unavailable}),
		Entry("missing operation rate", diskRates{ReadTimesSeconds: .01}, []string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable}),
		Entry("missing time rate preserves IOPS", diskRates{IOPSRead: 2}, []string{"2.00", Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable}),
		Entry("missing byte rate does not hide latency", diskRates{IOPSRead: 2, ReadTimesSeconds: .004}, []string{"2.00", Unavailable, Unavailable, Unavailable, Unavailable, "2.00", Unavailable, Unavailable, Unavailable, Unavailable}),
		Entry("active flushes", diskRates{FlushRequests: 4, FlushTimesSeconds: .02}, []string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, "4.00", "5.00"}),
		Entry("idle flushes", diskRates{FlushRequests: 0, FlushTimesSeconds: 0}, []string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, "0.00", noCompletedOperations}),
		Entry("missing flush request rate", diskRates{FlushTimesSeconds: .01}, []string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable}),
		Entry("missing flush time preserves flush rate", diskRates{FlushRequests: 2}, []string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, "2.00", Unavailable}),
		Entry("nil observation", nil, []string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable}),
		Entry("idle read and write counters show zero total IOPS and undefined latency", diskRates{IOPSRead: 0, IOPSWrite: 0, ReadTimesSeconds: 0, WriteTimesSeconds: 0}, []string{"0.00", "0.00", "0.00", Unavailable, Unavailable, noCompletedOperations, noCompletedOperations, noCompletedOperations, Unavailable, Unavailable}),
		Entry("an idle direction and active flushes do not change the read and write totals", diskRates{IOPSRead: 0, IOPSWrite: 4, ReadTimesSeconds: 0, WriteTimesSeconds: .012, FlushRequests: 2, FlushTimesSeconds: .01}, []string{"0.00", "4.00", "4.00", Unavailable, Unavailable, noCompletedOperations, "3.00", "3.00", "2.00", "5.00"}),
		Entry("missing write time preserves total IOPS but not total latency", diskRates{IOPSRead: 4, IOPSWrite: 2, ReadTimesSeconds: .008}, []string{"4.00", "2.00", "6.00", Unavailable, Unavailable, "2.00", Unavailable, Unavailable, Unavailable, Unavailable}),
	)

	DescribeTable("validate rate series and resource identity", func(change func(model.Vector) model.Vector, expected string, present bool) {
		vector := change(model.Vector{rateSample("data", 10)})
		rates := domainRates{}
		err := addDomainRates(rates, vector, IOPSRead, newFixture().vmi)
		if expected != "" {
			Expect(err).To(MatchError(ContainSubstring(expected)))
		} else {
			Expect(err).NotTo(HaveOccurred())
			if present {
				Expect(rates).To(Equal(domainRates{"data": diskRates{IOPSRead: 10}}))
			} else {
				Expect(rates).To(BeEmpty())
			}
		}
	},
		Entry("matching disk alias", func(v model.Vector) model.Vector { return v }, "", true),
		Entry("another namespace", func(v model.Vector) model.Vector { v[0].Metric[namespaceLabel] = "other"; return v }, "", false),
		Entry("another VMI", func(v model.Vector) model.Vector { v[0].Metric[nameLabel] = "other"; return v }, "", false),
		Entry("another node", func(v model.Vector) model.Vector { v[0].Metric[nodeLabel] = "worker-b"; return v }, "", false),
		Entry("missing disk", func(v model.Vector) model.Vector { delete(v[0].Metric, driveLabel); return v }, "", false),
		Entry("other VMI alongside selected VMI", func(v model.Vector) model.Vector {
			other := rateSample("data", 20)
			other.Metric[nameLabel] = "other"
			return append(v, other)
		}, "", true),
		Entry("ambiguous series", func(v model.Vector) model.Vector { return append(v, rateSample("data", 20)) }, "ambiguous monitoring rate", false),
		Entry("negative rate", func(v model.Vector) model.Vector { v[0].Value = -1; return v }, "invalid monitoring rate", false),
		Entry("NaN rate", func(v model.Vector) model.Vector { v[0].Value = model.SampleValue(math.NaN()); return v }, "invalid monitoring rate", false),
		Entry("infinite rate", func(v model.Vector) model.Vector { v[0].Value = model.SampleValue(math.Inf(1)); return v }, "invalid monitoring rate", false),
		Entry("histogram instead of scalar rate", func(v model.Vector) model.Vector { v[0].Histogram = &model.SampleHistogram{}; return v }, "invalid monitoring rate", false),
		Entry("empty vector", func(model.Vector) model.Vector { return model.Vector{} }, "", false),
	)

	DescribeTable("preserve resource inspection when monitoring is unavailable", func(change func(*fixture), warning string) {
		f := newFixture()
		change(f)
		out, stderr, err := f.run("inspect", "demo")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("example.csi.test"))
		Expect(out).To(ContainSubstring("Domain performance (Prometheus): unavailable"))
		Expect(out).To(ContainSubstring(Unavailable))
		Expect(stderr).To(ContainSubstring(warning))
	},
		Entry("forbidden endpoint", func(f *fixture) { f.monitorStatus = http.StatusForbidden }, "cannot query"),
		Entry("unavailable endpoint", func(f *fixture) { f.monitorStatus = http.StatusBadGateway }, "cannot query"),
		Entry("missing rates", func(f *fixture) { f.rates = nil }, "no recent storage rates"),
		Entry("malformed response", func(f *fixture) { f.monitorBody = "bad{" }, "cannot query"),
		Entry("wrong result type", func(f *fixture) {
			f.monitorBody = `{"status":"success","data":{"resultType":"scalar","result":[1,"2"]}}`
		}, "expected an instant vector"),
		Entry("partial query result", func(f *fixture) {
			f.monitorBody = `{"status":"success","warnings":["partial response"],"data":{"resultType":"vector","result":[]}}`
		}, "incomplete monitoring result"),
		Entry("migration in progress", func(f *fixture) { f.vmi.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{} }, "migration is in progress"),
		Entry("not enough current VMI history", func(f *fixture) { f.vmi.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Minute)) }, "insufficient metric history"),
	)

	DescribeTable("bound the query window to the current VMI running segment", func(change func(*v1.VirtualMachineInstance, time.Time), expected time.Duration) {
		at := time.Unix(1700000000, 0)
		vmi := newFixture().vmi
		vmi.CreationTimestamp = metav1.NewTime(at.Add(-time.Hour))
		change(vmi, at)
		Expect(vmiMetricsWindow(vmi, at)).To(Equal(expected))
	},
		Entry("existing VMI", func(*v1.VirtualMachineInstance, time.Time) {}, monitoringWindow),
		Entry("recently recreated VMI", func(v *v1.VirtualMachineInstance, t time.Time) {
			v.CreationTimestamp = metav1.NewTime(t.Add(-30 * time.Second))
		}, 30*time.Second),
		Entry("recent running transition", func(v *v1.VirtualMachineInstance, t time.Time) {
			v.Status.PhaseTransitionTimestamps = []v1.VirtualMachineInstancePhaseTransitionTimestamp{{Phase: v1.Running, PhaseTransitionTimestamp: metav1.NewTime(t.Add(-20 * time.Second))}}
		}, 20*time.Second),
		Entry("recent completed migration", func(v *v1.VirtualMachineInstance, t time.Time) {
			end := metav1.NewTime(t.Add(-10 * time.Second))
			v.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{Completed: true, EndTimestamp: &end}
		}, 10*time.Second),
		Entry("old completed migration", func(v *v1.VirtualMachineInstance, t time.Time) {
			end := metav1.NewTime(t.Add(-time.Hour))
			v.Status.MigrationState = &v1.VirtualMachineInstanceMigrationState{Completed: true, EndTimestamp: &end}
		}, monitoringWindow),
	)

	It("queries all eight counters at one evaluation time, with rates, identity and freshness filters", func() {
		at := time.Now().Truncate(time.Second)
		times := []string{}
		queries := []string{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			Expect(r.ParseForm()).To(Succeed())
			queries = append(queries, r.Form.Get("query"))
			times = append(times, r.Form.Get("time"))
			writeRates(w, r, domainRates{"data": diskRates{IOPSRead: 4, ReadTimesSeconds: .008, FlushRequests: 2, FlushTimesSeconds: .01}})
		}))
		defer server.Close()
		level := domainLevel{source: testPrometheusSource(server.URL)}
		rates, err := level.query(context.Background(), newFixture().vmi, at, monitoringWindow)
		Expect(err).NotTo(HaveOccurred())
		Expect(rates["data"]).To(Equal(diskRates{IOPSRead: 4, ReadTimesSeconds: .008, FlushRequests: 2, FlushTimesSeconds: .01}))
		Expect(queries).To(HaveLen(8))
		Expect(times).To(HaveLen(8))
		for i, q := range queries {
			Expect(q).To(HavePrefix("rate(kubevirt_vmi_storage_"))
			Expect(q).To(ContainSubstring(`namespace="storage-test",name="demo",node="worker-a",drive!=""`))
			Expect(q).To(ContainSubstring("[300s]) and present_over_time("))
			Expect(q).To(HaveSuffix("[120s])"))
			Expect(times[i]).To(Equal(times[0]))
		}
	})

	DescribeTable("use disk aliases without guessing guest device associations", func(drive string, includeWrites bool, expected []string) {
		f := newFixture()
		rates := diskRates{IOPSRead: 10, ReadTimesSeconds: .02, FlushRequests: 2, FlushTimesSeconds: .01}
		if includeWrites {
			rates[IOPSWrite], rates[WriteTimesSeconds] = 2, .01
		}
		f.rates = domainRates{drive: rates}
		out, stderr, err := f.run("inspect", "demo")
		Expect(err).NotTo(HaveOccurred())
		Expect(stderr).To(BeEmpty())
		Expect(out).To(ContainSubstring("Domain performance (Prometheus, lookback 5m0s, evaluated at"))
		lines := strings.Split(strings.SplitN(out, "\nGuest filesystems", 2)[0], "\n")
		fields := strings.Fields(lines[len(lines)-2])
		Expect(normalizedReportLines(out)).To(ContainElement("DISK READ IOPS WRITE IOPS TOTAL IOPS READ MiB/s WRITE MiB/s READ LATENCY ms WRITE LATENCY ms TOTAL LATENCY ms FLUSH OPS/s FLUSH LATENCY ms"))
		Expect(fields).To(HaveLen(11))
		Expect(fields[0]).To(Equal("data"))
		Expect(fields[1:]).To(Equal(expected))
	},
		Entry("VMI disk alias with only read rates", "data", false, []string{"10.00", Unavailable, Unavailable, Unavailable, Unavailable, "2.00", Unavailable, Unavailable, "2.00", "5.00"}),
		Entry("VMI disk alias with read and write rates", "data", true, []string{"10.00", "2.00", "12.00", Unavailable, Unavailable, "2.00", "5.00", "2.50", "2.00", "5.00"}),
		Entry("unmapped target name", "vdb", false, []string{Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable, Unavailable}),
	)

	It("propagates cancellation without hiding the resource report", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { Fail("a cancelled request must not reach monitoring") }))
		defer server.Close()
		level := domainLevel{source: testPrometheusSource(server.URL)}
		cmd := &cobra.Command{}
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetContext(ctx)
		Expect(level.print(cmd, newFixture().vmi, []diskInfo{{name: "data"}})).To(MatchError(context.Canceled))
	})

	It("exposes the monitoring URL and CA flags", func() {
		command := NewCommand()
		inspect, _, err := command.Find([]string{"inspect"})
		Expect(err).NotTo(HaveOccurred())
		Expect(inspect.Flags().Lookup(prometheusURLFlag)).NotTo(BeNil())
		Expect(inspect.Flags().Lookup(prometheusCAFlag)).NotTo(BeNil())
		Expect(inspect.Flags().Lookup("interval")).To(BeNil())
	})
})
