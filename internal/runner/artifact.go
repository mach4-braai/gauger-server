package runner

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// ArtifactName is the fallback artifact gauger uploads for a job.
func ArtifactName(checkRunID int64) string {
	return fmt.Sprintf("gauger-%d", checkRunID)
}

// ReadArtifact decodes the fallback artifact zip. Each file in it is one
// ExportMetricsServiceRequest in protobuf.
func ReadArtifact(zipData []byte) ([]*colmetrics.ExportMetricsServiceRequest, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("open artifact zip: %w", err)
	}
	var out []*colmetrics.ExportMetricsServiceRequest
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		data, err := io.ReadAll(io.LimitReader(rc, MaxBody+1))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		if len(data) > MaxBody {
			return nil, fmt.Errorf("%s: %w", f.Name, ErrTooLarge)
		}
		req := &colmetrics.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(data, req); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		out = append(out, req)
	}
	return out, nil
}
