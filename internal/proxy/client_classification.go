package proxy

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/local/claude-relay/internal/metrics"
)

const clientClassificationVersion = 6

const (
	clientClassCompatible  = "compatible"
	clientClassCCCandidate = "cc_candidate"
	clientClassAmbiguous   = "ambiguous"

	clientKindCompatible         = "compatible"
	clientKindMessages           = "messages"
	clientKindCountTokens        = "count_tokens"
	clientKindTokenCountFallback = "token_count_fallback"
	clientKindHaikuProbe         = "haiku_probe"
	clientKindAmbiguous          = "ambiguous"
)

var (
	claudeCodeUserAgentPattern  = regexp.MustCompile(`(?i)^claude-cli/\d+\.\d+\.\d+`)
	legacyMetadataUserIDPattern = regexp.MustCompile(`^user_[a-fA-F0-9]{64}_account_[a-fA-F0-9-]*_session_[a-fA-F0-9-]{36}$`)
)

var claudeCodeSystemPromptMarkers = []string{
	"You are Claude Code, Anthropic's official CLI for Claude.",
	"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
	"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
	"You are a file search specialist for Claude Code, Anthropic's official CLI for Claude.",
	"You are a helpful AI assistant tasked with summarizing conversations.",
	"You are an interactive CLI tool that helps users",
}

const (
	claudeCodeSecurityMonitorPromptPrefix = "You are a security monitor for autonomous AI coding agents."
	claudeCodeSecurityMonitorPromptMinLen = 10_000
)

var claudeCodeSecurityMonitorMarkers = []string{
	"## Threat Model",
	"- `<transcript>`:",
	"## HARD BLOCK",
	"## SOFT BLOCK",
	"## Classification Process",
	"## Output Format",
	"<block>yes</block>",
	"<block>no</block>",
}

type clientEvidence struct {
	BillingBlock       bool
	CCVersion          bool
	KnownEntrypoint    bool
	OfficialPrompt     bool
	StructuredMetadata bool
	ClaudeUserAgent    bool
	ClaudeCodeSession  bool
	XAppCLI            bool
	AnthropicBeta      bool
	AnthropicVersion   bool
}

type clientObservation struct {
	Class       string
	Kind        string
	Version     int
	Evidence    clientEvidence
	Consistency *metrics.ClientConsistency
}

func classifyClient(root map[string]any, headers http.Header, path string) clientObservation {
	evidence := billingEvidence(root["system"])
	evidence.OfficialPrompt = hasClaudeCodeSystemPrompt(root["system"])
	evidence.StructuredMetadata = hasStructuredMetadata(root["metadata"])
	setHeaderEvidence(&evidence, headers)

	observation := clientObservation{
		Class:    clientClassCompatible,
		Kind:     clientKindCompatible,
		Version:  clientClassificationVersion,
		Evidence: evidence,
	}
	if evidence.ClaudeUserAgent || evidence.BillingBlock {
		observation.Consistency = inspectClientConsistency(root, headers)
	}
	if evidence.hasCompleteHeaders() {
		switch {
		case path == "/v1/messages/count_tokens":
			observation.Class = clientClassCCCandidate
			observation.Kind = clientKindCountTokens
			return observation
		case isHaikuProbe(root):
			observation.Class = clientClassCCCandidate
			observation.Kind = clientKindHaikuProbe
			return observation
		case isTokenCountFallback(root):
			observation.Class = clientClassCCCandidate
			observation.Kind = clientKindTokenCountFallback
			return observation
		case path == "/v1/messages" && evidence.StructuredMetadata &&
			((evidence.BillingBlock && evidence.CCVersion && evidence.KnownEntrypoint) || evidence.OfficialPrompt):
			observation.Class = clientClassCCCandidate
			observation.Kind = clientKindMessages
			return observation
		}
	}

	if hasAnyClientEvidence(evidence) {
		observation.Class = clientClassAmbiguous
		observation.Kind = clientKindAmbiguous
	}
	return observation
}

// classifyClientHeaders classifies a request from its headers alone. Every
// cc_candidate path requires complete headers, so a request without them can
// be rejected by the official ingress before its body is read.
func classifyClientHeaders(headers http.Header) clientObservation {
	var evidence clientEvidence
	setHeaderEvidence(&evidence, headers)
	observation := clientObservation{
		Class:    clientClassCompatible,
		Kind:     clientKindCompatible,
		Version:  clientClassificationVersion,
		Evidence: evidence,
	}
	if hasAnyClientEvidence(evidence) {
		observation.Class = clientClassAmbiguous
		observation.Kind = clientKindAmbiguous
	}
	return observation
}

func setHeaderEvidence(evidence *clientEvidence, headers http.Header) {
	evidence.ClaudeUserAgent = claudeCodeUserAgentPattern.MatchString(strings.TrimSpace(headers.Get("User-Agent")))
	evidence.ClaudeCodeSession = strings.TrimSpace(headers.Get(claudeCodeSessionHeader)) != ""
	// Claude Code's /fork background sessions send cli-bg instead of cli.
	xApp := strings.TrimSpace(headers.Get("X-App"))
	evidence.XAppCLI = strings.EqualFold(xApp, "cli") || strings.EqualFold(xApp, "cli-bg")
	evidence.AnthropicBeta = strings.TrimSpace(headers.Get("anthropic-beta")) != ""
	evidence.AnthropicVersion = strings.TrimSpace(headers.Get("anthropic-version")) != ""
}

func (evidence clientEvidence) hasCompleteHeaders() bool {
	return evidence.ClaudeUserAgent && evidence.ClaudeCodeSession && evidence.XAppCLI &&
		evidence.AnthropicBeta && evidence.AnthropicVersion
}

func hasAnyClientEvidence(evidence clientEvidence) bool {
	return evidence.BillingBlock || evidence.CCVersion || evidence.KnownEntrypoint || evidence.OfficialPrompt ||
		evidence.StructuredMetadata || evidence.ClaudeUserAgent || evidence.ClaudeCodeSession || evidence.XAppCLI ||
		evidence.AnthropicBeta || evidence.AnthropicVersion
}

func billingEvidence(value any) clientEvidence {
	var evidence clientEvidence
	for _, text := range systemTexts(value) {
		trimmed := strings.TrimSpace(text)
		if !strings.HasPrefix(trimmed, billingAttributionPrefix) {
			continue
		}
		evidence.BillingBlock = true
		fields := parseBillingFields(strings.TrimSpace(strings.TrimPrefix(trimmed, billingAttributionPrefix)))
		evidence.CCVersion = evidence.CCVersion || strings.TrimSpace(fields["cc_version"]) != ""
		// Entrypoint values legitimately vary between CLI, IDE, Desktop, and Agent SDK releases.
		// Presence is useful as a request-shape signal; the authenticated ingress key remains the trust boundary.
		evidence.KnownEntrypoint = evidence.KnownEntrypoint || strings.TrimSpace(fields["cc_entrypoint"]) != ""
	}
	return evidence
}

func systemTexts(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		texts := make([]string, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			text, ok := block["text"].(string)
			if ok {
				texts = append(texts, text)
			}
		}
		return texts
	default:
		return nil
	}
}

func parseBillingFields(value string) map[string]string {
	fields := make(map[string]string)
	for _, raw := range strings.Split(value, ";") {
		key, fieldValue, found := strings.Cut(raw, "=")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			fields[key] = strings.TrimSpace(fieldValue)
		}
	}
	return fields
}

func hasClaudeCodeSystemPrompt(value any) bool {
	for _, text := range systemTexts(value) {
		for _, marker := range claudeCodeSystemPromptMarkers {
			if strings.Contains(text, marker) {
				return true
			}
		}
		if len(text) < claudeCodeSecurityMonitorPromptMinLen ||
			!strings.HasPrefix(text, claudeCodeSecurityMonitorPromptPrefix) {
			continue
		}
		matched := true
		for _, marker := range claudeCodeSecurityMonitorMarkers {
			if !strings.Contains(text, marker) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func hasStructuredMetadata(value any) bool {
	metadata, ok := value.(map[string]any)
	if !ok {
		return false
	}
	userID, ok := metadata["user_id"].(string)
	if !ok {
		return false
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	if legacyMetadataUserIDPattern.MatchString(userID) {
		return true
	}
	var identity struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(userID), &identity); err != nil {
		return false
	}
	// API-key mode legitimately sends an empty account_uuid. Device and session are the stable fields.
	return strings.TrimSpace(identity.DeviceID) != "" && strings.TrimSpace(identity.SessionID) != ""
}

func isTokenCountFallback(root map[string]any) bool {
	if !maxTokensEqualsOne(root["max_tokens"]) || streamEnabled(root["stream"]) || len(systemTexts(root["system"])) != 0 {
		return false
	}
	model, _ := root["model"].(string)
	return strings.TrimSpace(model) != "" && hasStructuredMetadata(root["metadata"])
}

func isHaikuProbe(root map[string]any) bool {
	if !maxTokensEqualsOne(root["max_tokens"]) || streamEnabled(root["stream"]) || len(systemTexts(root["system"])) != 0 {
		return false
	}
	model, _ := root["model"].(string)
	return strings.Contains(strings.ToLower(model), "haiku")
}

func maxTokensEqualsOne(value any) bool {
	switch typed := value.(type) {
	case json.Number:
		return typed.String() == "1"
	case float64:
		return typed == 1
	case int:
		return typed == 1
	default:
		return false
	}
}

func streamEnabled(value any) bool {
	stream, _ := value.(bool)
	return stream
}
