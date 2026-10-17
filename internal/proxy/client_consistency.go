package proxy

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/local/claude-relay/internal/metrics"
)

var (
	billingVersionPattern = regexp.MustCompile(`^(\d+\.\d+\.\d+)(?:\.(.+))?$`)
	// Older UAs omit entrypoint information or contain OS details instead. Only
	// the explicit external-entrypoint form supplies a value we can compare.
	claudeCodeUAEntrypointPattern = regexp.MustCompile(`(?i)^claude-cli/\d+\.\d+\.\d+ \(external,\s*([a-z0-9_-]+)\)(?:\s|$)`)
)

// inspectClientConsistency compares claims without retaining any identifiers.
// Missing auxiliary-request fields are not contradictions. Shape classification
// separately decides which fields each request kind actually needs.
func inspectClientConsistency(root map[string]any, headers http.Header) *metrics.ClientConsistency {
	result := &metrics.ClientConsistency{
		Session: "not_present", Version: "not_present", Entrypoint: "not_comparable", Checksum: "not_present",
	}
	session := strings.TrimSpace(headers.Get(claudeCodeSessionHeader))
	metadataSession := originalMetadataSession(root["metadata"])
	if session != "" && metadataSession != "" {
		result.Session = "matched"
		if session != metadataSession {
			result.Session = "mismatched"
			result.Violations = append(result.Violations, "session_mismatch")
		}
	}
	uaVersion := extractClaudeCodeUAVersion(headers.Get("User-Agent"))
	uaEntrypoint := ""
	if match := claudeCodeUAEntrypointPattern.FindStringSubmatch(strings.TrimSpace(headers.Get("User-Agent"))); len(match) == 2 {
		uaEntrypoint = match[1]
	}
	for _, text := range systemTexts(root["system"]) {
		text = strings.TrimSpace(text)
		if !strings.HasPrefix(text, billingAttributionPrefix) {
			continue
		}
		// Inspect every occurrence, rather than letting duplicate fields/blocks
		// hide an earlier contradictory claim behind a last-value-wins parser.
		for _, raw := range strings.Split(strings.TrimPrefix(text, billingAttributionPrefix), ";") {
			key, value, found := strings.Cut(raw, "=")
			if !found {
				continue
			}
			value = strings.TrimSpace(value)
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "cch":
				// Genuine OAuth/Vertex traffic can carry CCH. Presence alone is
				// not evidence of fabrication; retain no value and never reject it.
				result.CCH = true
			case "cc_version":
				match := billingVersionPattern.FindStringSubmatch(value)
				if len(match) != 3 || uaVersion == "" || match[1] != uaVersion {
					result.Version = "mismatched"
					continue
				}
				if result.Version != "mismatched" {
					result.Version = "matched"
				}
				status := observeVersionChecksum(root, match[1], match[2])
				if result.Checksum != "mismatched" {
					result.Checksum = status
				}
			case "cc_entrypoint":
				if uaEntrypoint == "" {
					continue
				}
				if result.Entrypoint != "mismatched" {
					result.Entrypoint = "matched"
				}
				if canonicalEntrypoint(value) != canonicalEntrypoint(uaEntrypoint) {
					result.Entrypoint = "mismatched"
				}
			}
		}
	}
	if result.Version == "mismatched" {
		result.Violations = append(result.Violations, "version_mismatch")
	}
	if result.Entrypoint == "mismatched" {
		result.Violations = append(result.Violations, "entrypoint_mismatch")
	}
	return result
}

func originalMetadataSession(value any) string {
	identity := parseMetadataIdentity(value)
	if identity.SessionID != "" {
		return identity.SessionID
	}
	metadata, _ := value.(map[string]any)
	userID, _ := metadata["user_id"].(string)
	userID = strings.TrimSpace(userID)
	if legacyMetadataUserIDPattern.MatchString(userID) {
		return userID[strings.LastIndex(userID, "_session_")+len("_session_"):]
	}
	return ""
}

func canonicalEntrypoint(value string) string {
	// The captured Desktop third-party API attribution uses this suffix while
	// Desktop UAs can keep the original surface name. It is an auth-path alias.
	if value == "claude-desktop-3p" {
		return "claude-desktop"
	}
	return value
}

func observeVersionChecksum(root map[string]any, version, suffix string) string {
	if suffix == "" {
		return "not_present"
	}
	if version != "2.1.286" && version != "2.1.295" {
		return "unsupported_version"
	}
	messages, _ := root["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		var texts []string
		switch content := message["content"].(type) {
		case string:
			texts = []string{content}
		case []any:
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				if text, ok := block["text"].(string); ok && block["type"] == "text" {
					texts = append(texts, text)
				}
			}
		}
		if len(texts) == 0 {
			return "unavailable"
		}
		// Native 2.1.286/295 use the first non-meta user text. Wire content can
		// prepend client reminder blocks, whose provenance is lost in JSON. Try
		// each text block for diagnostics only; this is deliberately not admission.
		for _, text := range texts {
			if nativeVersionChecksum(text, version) == strings.ToLower(suffix) {
				return "matched"
			}
		}
		return "mismatched"
	}
	return "unavailable"
}

func nativeVersionChecksum(text, version string) string {
	// These releases sample JS string positions (UTF-16 code units), not UTF-8
	// byte offsets. Decode after sampling to match JS replacement of unpaired
	// surrogates when createHash encodes the resulting string as UTF-8.
	units := utf16.Encode([]rune(text))
	var sampled []uint16
	for _, position := range []int{4, 7, 20} {
		if position >= len(units) {
			sampled = append(sampled, '0')
		} else {
			sampled = append(sampled, units[position])
		}
	}
	// This client-embedded constant is public material, not an authentication key.
	digest := sha256.Sum256([]byte("59cf53e54c78" + string(utf16.Decode(sampled)) + version))
	return fmt.Sprintf("%x", digest)[:3]
}
