package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func gzipFixture(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCompressionForward(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for _, tc := range []struct {
			name                          string
			incomingGzip, disabled, large bool
		}{
			{name: "small gzip ingress", incomingGzip: true},
			{name: "large identity ingress", large: true},
			{name: "large gzip ingress", incomingGzip: true, large: true},
			{name: "disabled with gzip ingress", incomingGzip: true, disabled: true, large: true},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				// Carry binary image bytes through JSON/base64 and both compression directions.
				image := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aXioAAAAASUVORK5CYII="
				text := "hello"
				if tc.large {
					text = strings.Repeat("compression probe ", 1000)
				}
				messages := []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": text},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": image}},
				}}}
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var reader io.Reader = r.Body
					compressed := tc.large && !tc.disabled
					if got := r.Header.Get("Content-Encoding"); (got == "gzip") != compressed || (!compressed && got != "") {
						t.Errorf("encoding=%q compressed=%v", got, compressed)
					}
					if compressed {
						gz, err := gzip.NewReader(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						defer gz.Close()
						reader = gz
					}
					raw, err := io.ReadAll(reader)
					if err != nil {
						t.Error(err)
					}
					var body map[string]any
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Error(err)
					}
					want, _ := json.Marshal(messages)
					got, _ := json.Marshal(body["messages"])
					if !bytes.Equal(want, got) {
						t.Error("message/image bytes changed")
					}
					if _, ok := body["system"]; !ok {
						t.Error("missing attribution after decoding")
					}
					w.Header().Set("Content-Type", "application/json")
					if strings.HasSuffix(path, "count_tokens") {
						io.WriteString(w, `{"input_tokens":42}`)
					} else {
						io.WriteString(w, `{"type":"message","usage":{"input_tokens":42,"output_tokens":1}}`)
					}
				}))
				defer upstream.Close()
				server := newTestServer(t, upstream.URL, 1<<20)
				server.cfg.DisableUpstreamCompression = tc.disabled
				raw, _ := json.Marshal(map[string]any{"model": "claude-test", "messages": messages, "max_tokens": 4})
				if tc.incomingGzip {
					raw = gzipFixture(t, raw)
				}
				req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
				req.Header.Set("x-api-key", "downstream-key")
				if tc.incomingGzip {
					req.Header.Set("Content-Encoding", "gzip")
				}
				rec := httptest.NewRecorder()
				server.routes().ServeHTTP(rec, req)
				if rec.Code != 200 || calls != 1 {
					t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body)
				}
			})
		}
	}
}

func TestCompressionRejectsInvalidIngress(t *testing.T) {
	valid := gzipFixture(t, []byte(`{"messages":[]}`))
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 255
	cases := []struct {
		name, encoding string
		body           []byte
		limit          int64
		status         int
	}{
		{"unsupported", "br", []byte("abc"), 1024, 415},
		{"stacked", "gzip, gzip", valid, 1024, 415},
		{"invalid header", "gzip", []byte("not gzip"), 1024, 400},
		{"truncated", "gzip", valid[:len(valid)-3], 1024, 400},
		{"checksum", "gzip", corrupt, 1024, 400},
		{"expanded limit", "gzip", gzipFixture(t, bytes.Repeat([]byte("a"), 2048)), 1024, 413},
		{"wire limit", "gzip", valid, 20, 413},
		{"identity limit", "", bytes.Repeat([]byte("a"), 2048), 1024, 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request reached upstream") }))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, tc.limit)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(tc.body))
			req.Header.Set("x-api-key", "downstream-key")
			req.Header.Set("Content-Encoding", tc.encoding)
			rec := httptest.NewRecorder()
			server.routes().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body)
			}
		})
	}
}

func TestUpstreamCompressionSelection(t *testing.T) {
	for _, size := range []int{upstreamCompressionThreshold, upstreamCompressionThreshold + 1} {
		body := bytes.Repeat([]byte("x"), size)
		encoded, compressed, err := encodeUpstreamBody(body, false)
		if err != nil {
			t.Fatal(err)
		}
		if compressed != (size > upstreamCompressionThreshold) {
			t.Fatalf("size=%d compressed=%v", size, compressed)
		}
		if !compressed && !bytes.Equal(encoded, body) {
			t.Fatal("identity body changed")
		}
	}
	// Pseudorandom bytes represent data that gzip cannot reduce.
	body := make([]byte, 8192)
	rng := rand.New(rand.NewSource(1))
	if _, err := rng.Read(body); err != nil {
		t.Fatal(err)
	}
	encoded, compressed, err := encodeUpstreamBody(body, false)
	if err != nil || compressed || !bytes.Equal(body, encoded) {
		t.Fatalf("incompressible body compressed=%v err=%v", compressed, err)
	}
}
