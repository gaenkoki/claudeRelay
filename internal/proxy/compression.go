package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
)

const upstreamCompressionThreshold = 4 << 10

// Limit both wire bytes and decoded bytes: a small gzip payload may expand beyond
// the configured JSON limit. Reading to EOF also verifies gzip trailers/checksums.
func readRequestBody(w http.ResponseWriter, body io.ReadCloser, encoding string, limit int64) ([]byte, error) {
	wire := http.MaxBytesReader(w, body, limit)
	defer wire.Close()
	var decoded io.Reader = wire
	if encoding == "gzip" {
		reader, err := gzip.NewReader(wire)
		if err != nil {
			return nil, fmt.Errorf("decode gzip request body: %w", err)
		}
		defer reader.Close()
		decoded = reader
	}
	limited := http.MaxBytesReader(w, io.NopCloser(decoded), limit)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return data, nil
}

func encodeUpstreamBody(body []byte, disabled bool) ([]byte, bool, error) {
	if disabled || len(body) <= upstreamCompressionThreshold {
		return body, false, nil
	}
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, 6)
	if err != nil {
		return nil, false, fmt.Errorf("create upstream gzip writer: %w", err)
	}
	if _, err := writer.Write(body); err != nil {
		return nil, false, fmt.Errorf("compress upstream body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, false, fmt.Errorf("finish upstream gzip body: %w", err)
	}
	// Already compressed data can grow under gzip; send its original bytes instead.
	if buffer.Len() >= len(body) {
		return body, false, nil
	}
	return buffer.Bytes(), true, nil
}
