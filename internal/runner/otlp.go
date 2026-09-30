package runner

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"sort"
	"strconv"
	"strings"
	"time"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	metrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// MaxBody bounds an OTLP request body after decompression.
const MaxBody = 16 << 20

var ErrTooLarge = errors.New("request body too large")

// IsJSON reports whether an OTLP/HTTP Content-Type is the JSON encoding.
func IsJSON(contentType string) bool {
	mt, _, _ := mime.ParseMediaType(contentType)
	return mt == "application/json"
}

// DecodeRequest reads an OTLP/HTTP metrics body in protobuf or JSON,
// optionally gzip-compressed.
func DecodeRequest(body io.Reader, contentType, contentEncoding string) (*colmetrics.ExportMetricsServiceRequest, error) {
	switch contentEncoding {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		body = zr
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", contentEncoding)
	}
	data, err := io.ReadAll(io.LimitReader(body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBody {
		return nil, ErrTooLarge
	}
	req := &colmetrics.ExportMetricsServiceRequest{}
	if IsJSON(contentType) {
		err = protojson.Unmarshal(data, req)
	} else {
		err = proto.Unmarshal(data, req)
	}
	if err != nil {
		return nil, fmt.Errorf("decode OTLP: %w", err)
	}
	return req, nil
}

// Point is one numeric sample.
type Point struct {
	Identity Identity
	Metric   string
	// Series is the sorted non-identity data point attributes as k=v,k=v.
	Series string
	Time   time.Time
	Value  float64
}

// Points flattens gauge and sum data points. It counts points it cannot use,
// such as histograms or points without a valid identity, as rejected.
func Points(req *colmetrics.ExportMetricsServiceRequest) (points []Point, rejected int64) {
	for _, rm := range req.GetResourceMetrics() {
		resAttrs := attrMap(rm.GetResource().GetAttributes())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				var dps []*metrics.NumberDataPoint
				switch d := m.GetData().(type) {
				case *metrics.Metric_Gauge:
					dps = d.Gauge.GetDataPoints()
				case *metrics.Metric_Sum:
					dps = d.Sum.GetDataPoints()
				default:
					rejected += countPoints(m)
					continue
				}
				for _, dp := range dps {
					p, ok := point(m.GetName(), resAttrs, dp)
					if !ok {
						rejected++
						continue
					}
					points = append(points, p)
				}
			}
		}
	}
	return points, rejected
}

func point(name string, resAttrs map[string]string, dp *metrics.NumberDataPoint) (Point, bool) {
	if name == "" || dp.GetTimeUnixNano() == 0 {
		return Point{}, false
	}
	dpAttrs := attrMap(dp.GetAttributes())
	id, err := ParseIdentity(func(k string) string {
		if v, ok := dpAttrs[k]; ok {
			return v
		}
		return resAttrs[k]
	})
	if err != nil {
		return Point{}, false
	}
	var v float64
	switch x := dp.GetValue().(type) {
	case *metrics.NumberDataPoint_AsDouble:
		v = x.AsDouble
	case *metrics.NumberDataPoint_AsInt:
		v = float64(x.AsInt)
	default:
		return Point{}, false
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return Point{}, false
	}
	keys := make([]string, 0, len(dpAttrs))
	for k := range dpAttrs {
		if !isIdentityKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + dpAttrs[k]
	}
	return Point{
		Identity: id,
		Metric:   name,
		Series:   strings.Join(parts, ","),
		Time:     time.Unix(0, int64(dp.GetTimeUnixNano())).UTC(),
		Value:    v,
	}, true
}

func countPoints(m *metrics.Metric) int64 {
	switch d := m.GetData().(type) {
	case *metrics.Metric_Histogram:
		return int64(len(d.Histogram.GetDataPoints()))
	case *metrics.Metric_ExponentialHistogram:
		return int64(len(d.ExponentialHistogram.GetDataPoints()))
	case *metrics.Metric_Summary:
		return int64(len(d.Summary.GetDataPoints()))
	}
	return 0
}

func attrMap(kvs []*common.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		switch v := kv.GetValue().GetValue().(type) {
		case *common.AnyValue_StringValue:
			out[kv.GetKey()] = v.StringValue
		case *common.AnyValue_IntValue:
			out[kv.GetKey()] = strconv.FormatInt(v.IntValue, 10)
		case *common.AnyValue_DoubleValue:
			out[kv.GetKey()] = strconv.FormatFloat(v.DoubleValue, 'f', -1, 64)
		case *common.AnyValue_BoolValue:
			out[kv.GetKey()] = strconv.FormatBool(v.BoolValue)
		}
	}
	return out
}
