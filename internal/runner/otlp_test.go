package runner_test

import (
	"bytes"
	"compress/gzip"
	"testing"
	"time"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	metrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/mach4-braai/gauger-server/internal/runner"
)

func str(k, v string) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v int64) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: v}}}
}

var t0 = time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

func request() *colmetrics.ExportMetricsServiceRequest {
	return &colmetrics.ExportMetricsServiceRequest{ResourceMetrics: []*metrics.ResourceMetrics{{
		Resource: &resource.Resource{Attributes: []*common.KeyValue{
			num(runner.AttrRunID, 100), num(runner.AttrRunAttempt, 1), num(runner.AttrCheckRunID, 555),
			str(runner.AttrRepository, "acme/app"), str(runner.AttrWorkflow, "CI"), str(runner.AttrJob, "build"),
			str(runner.AttrRunnerName, "GitHub Actions 3"),
		}},
		ScopeMetrics: []*metrics.ScopeMetrics{{Metrics: []*metrics.Metric{
			{Name: "system.memory.usage", Data: &metrics.Metric_Gauge{Gauge: &metrics.Gauge{DataPoints: []*metrics.NumberDataPoint{{
				TimeUnixNano: uint64(t0.UnixNano()),
				Attributes:   []*common.KeyValue{str("system.memory.state", "used"), str("b", "2")},
				Value:        &metrics.NumberDataPoint_AsInt{AsInt: 1 << 30},
			}}}}},
			{Name: "system.cpu.utilization", Data: &metrics.Metric_Gauge{Gauge: &metrics.Gauge{DataPoints: []*metrics.NumberDataPoint{{
				TimeUnixNano: uint64(t0.UnixNano()),
				Attributes:   []*common.KeyValue{num(runner.AttrCheckRunID, 556)},
				Value:        &metrics.NumberDataPoint_AsDouble{AsDouble: 0.5},
			}}}}},
			{Name: "latency", Data: &metrics.Metric_Histogram{Histogram: &metrics.Histogram{DataPoints: []*metrics.HistogramDataPoint{{}, {}}}}},
		}}},
	}}}
}

func TestPoints(t *testing.T) {
	points, rejected := runner.Points(request())
	if rejected != 2 {
		t.Errorf("rejected = %d, want the 2 histogram points", rejected)
	}
	if len(points) != 2 {
		t.Fatalf("points = %d, want 2", len(points))
	}
	mem := points[0]
	if mem.Series != "b=2,system.memory.state=used" || mem.Value != 1<<30 || !mem.Time.Equal(t0) || mem.Identity.CheckRunID != 555 {
		t.Errorf("memory point = %+v", mem)
	}
	if points[1].Identity.CheckRunID != 556 || points[1].Series != "" {
		t.Errorf("data point attributes should override resource identity and stay out of the series: %+v", points[1])
	}
}

func TestPointsRejectRecordsWithoutIdentity(t *testing.T) {
	req := request()
	req.ResourceMetrics[0].Resource.Attributes = []*common.KeyValue{num(runner.AttrRunID, 100), num(runner.AttrRunAttempt, 1), str(runner.AttrRepository, "acme/app")}
	points, rejected := runner.Points(req)
	if len(points) != 1 || rejected != 3 {
		t.Fatalf("points=%d rejected=%d, want only the point with its own check_run_id", len(points), rejected)
	}
}

func TestDecodeRequest(t *testing.T) {
	pb, _ := proto.Marshal(request())
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(pb)
	zw.Close()
	js, _ := protojson.Marshal(request())

	for _, tc := range []struct {
		name, ct, ce string
		body         []byte
	}{
		{"protobuf", "application/x-protobuf", "", pb},
		{"gzip protobuf", "application/x-protobuf", "gzip", gz.Bytes()},
		{"json", "application/json; charset=utf-8", "", js},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := runner.DecodeRequest(bytes.NewReader(tc.body), tc.ct, tc.ce)
			if err != nil {
				t.Fatal(err)
			}
			if points, _ := runner.Points(req); len(points) != 2 {
				t.Fatalf("points = %d, want 2", len(points))
			}
		})
	}
	if _, err := runner.DecodeRequest(bytes.NewReader(pb), "application/x-protobuf", "br"); err == nil {
		t.Error("unsupported encoding should fail")
	}
}
