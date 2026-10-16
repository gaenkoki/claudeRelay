package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const refusalFingerprintSecretSetting = "refusal_fingerprint_secret"

type refusalGuard struct {
	secret []byte
}

type refusalFingerprints struct {
	lookup []string
	record []string
}

func loadRefusalGuard(database *store.Store) (*refusalGuard, error) {
	value, found, err := database.RuntimeSetting(context.Background(), refusalFingerprintSecretSetting)
	if err != nil {
		return nil, err
	}
	if !found {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate refusal fingerprint secret: %w", err)
		}
		value = hex.EncodeToString(secret)
		if err := database.SetRuntimeSetting(context.Background(), refusalFingerprintSecretSetting, value); err != nil {
			return nil, err
		}
	}
	secret, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode persisted refusal fingerprint secret: %w", err)
	}
	if len(secret) != 32 {
		return nil, fmt.Errorf("invalid persisted refusal fingerprint secret: expected 32 bytes, got %d", len(secret))
	}
	return &refusalGuard{secret: secret}, nil
}

func (g *refusalGuard) digest(parts ...string) string {
	h := hmac.New(sha256.New, g.secret)
	for _, part := range parts {
		// Length framing prevents distinct tuples from producing the same input.
		fmt.Fprintf(h, "%d:", len(part))
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fingerprints runs on the original body, before attribution or tool rewriting.
// The content seed includes system/tools but never stands alone as a block key:
// identical harmless messages under different system instructions must not share
// a cooldown. Model/metadata/cache and billing annotations are not content.
func (g *refusalGuard) fingerprints(body []byte, headers http.Header, ingress string) (refusalFingerprints, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return refusalFingerprints{}, fmt.Errorf("decode refusal conversation: %w", err)
	}
	var result refusalFingerprints
	session := firstSessionHeader(headers)
	if session == "" {
		session = parseMetadataIdentity(root["metadata"]).SessionID
		if session == "" {
			// Older Claude Code clients encode the session in a flat user_id.
			metadata, _ := root["metadata"].(map[string]any)
			userID, _ := metadata["user_id"].(string)
			if legacyMetadataUserIDPattern.MatchString(userID) {
				session = userID[strings.LastIndex(userID, "_session_")+len("_session_"):]
			}
		}
	}
	if session != "" {
		key := "session:" + g.digest("session-v1", ingress, session)
		result.lookup = append(result.lookup, key)
		result.record = append(result.record, key)
	}
	messages, _ := root["messages"].([]any)
	system, _, err := inspectSystem(root["system"])
	if err != nil {
		return refusalFingerprints{}, err
	}
	normalizedSystem := make([]any, 0, len(system))
	for _, raw := range system {
		block, ok := raw.(map[string]any)
		if !ok {
			return refusalFingerprints{}, fmt.Errorf("system block must be an object")
		}
		text, _ := block["text"].(string)
		if strings.HasPrefix(text, billingAttributionPrefix) {
			continue
		}
		normalizedSystem = append(normalizedSystem, refusalContentBlock(block))
	}
	tools, _ := root["tools"].([]any)
	normalizedTools := make([]any, 0, len(tools))
	for _, raw := range tools {
		block, ok := raw.(map[string]any)
		if !ok {
			return refusalFingerprints{}, fmt.Errorf("tool definition must be an object")
		}
		normalizedTools = append(normalizedTools, refusalContentBlock(block))
	}
	seed, err := json.Marshal(map[string]any{"system": normalizedSystem, "tools": normalizedTools})
	if err != nil {
		return refusalFingerprints{}, fmt.Errorf("encode refusal context: %w", err)
	}
	chain := g.digest("conversation-v1", ingress, string(seed))
	lastRole := ""
	blockCount := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return refusalFingerprints{}, fmt.Errorf("conversation message must be an object")
		}
		role, _ := message["role"].(string)
		if role == "" {
			return refusalFingerprints{}, fmt.Errorf("conversation message role must be a nonempty string")
		}
		var blocks []any
		switch content := message["content"].(type) {
		case string:
			blocks = []any{map[string]any{"type": "text", "text": content}}
		case []any:
			blocks = content
		default:
			return refusalFingerprints{}, fmt.Errorf("conversation content must be a string or block array")
		}
		// Consecutive same-role messages are equivalent to concatenated blocks.
		if role != lastRole {
			chain = g.digest("role", chain, role)
			lastRole = role
		}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				return refusalFingerprints{}, fmt.Errorf("conversation content block must be an object")
			}
			encoded, err := json.Marshal(refusalContentBlock(block))
			if err != nil {
				return refusalFingerprints{}, fmt.Errorf("encode refusal conversation block: %w", err)
			}
			chain = g.digest("block", chain, string(encoded))
			blockCount++
			result.lookup = append(result.lookup, "chain:"+chain)
		}
	}
	if blockCount > 0 {
		result.record = append(result.record, "chain:"+chain)
	}
	return result, nil
}

func refusalContentBlock(block map[string]any) map[string]any {
	normalized := make(map[string]any, len(block))
	for key, value := range block {
		// Cache annotations describe transport caching, not message content.
		// Do not recursively strip user/tool data bearing the same field name.
		if key != "cache_control" {
			normalized[key] = value
		}
	}
	return normalized
}

func writeRefusalCooldown(w http.ResponseWriter, state store.RefusalCooldown, now time.Time) string {
	seconds := max(int64(1), (state.UntilAt-now.UnixMilli()+999)/1000)
	message := fmt.Sprintf("该会话已触发 refusal，暂时无法继续对话，请等待 %d 分 %d 秒后重试。", seconds/60, seconds%60)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":                "permission_error",
			"code":                "refusal_cooldown",
			"message":             message,
			"retry_after_seconds": seconds,
			"cooldown_until":      time.UnixMilli(state.UntilAt).UTC().Format(time.RFC3339Nano),
		},
	})
	return message
}
