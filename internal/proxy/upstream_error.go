package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/local/claude-relay/internal/metrics"
)

// Recognize only the observed upstream error envelope and known message prefixes.
// Third-party responses may be malformed, oversized, or contain echoed user data;
// unmatched responses stay uncategorized and are forwarded unchanged. Only the
// fixed category is retained, never the upstream message or response body.
func classifyUpstreamError(status int, preview upstreamErrorPreview) string {
	if status != http.StatusBadRequest || preview.truncated {
		return ""
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(preview.body, &envelope) != nil || envelope.Type != "error" || envelope.Error.Type != "invalid_request_error" {
		return ""
	}
	for _, prefix := range []string{
		"Third-party apps now draw from extra usage, not plan limits.",
		"Third-party apps now draw from your extra usage, not your plan limits.",
	} {
		if strings.HasPrefix(envelope.Error.Message, prefix) {
			return metrics.UpstreamErrorThirdPartyExtraUsage
		}
	}
	return ""
}
