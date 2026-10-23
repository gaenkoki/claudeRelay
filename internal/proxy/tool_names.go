package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const mcpToolNamePrefix = "mcp__"

// toolNameMapping contains only names changed by the experimental ingress.
// A client that already declared an mcp__ name therefore never has that name
// stripped from the response.
type toolNameMapping map[string]string

// normalizeExperimentalToolNames adapts third-party custom tool names to the
// MCP-shaped names that avoid observed subscription tool-name classification.
// Anthropic server tools carry a versioned type and require their fixed names, so
// they are never renamed. References in tool_choice and prior tool_use blocks are changed
// only when their matching custom declaration was renamed.
func normalizeExperimentalToolNames(body []byte) ([]byte, int, toolNameMapping, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, 0, nil, fmt.Errorf("decode request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, 0, nil, fmt.Errorf("decode request body: trailing JSON value")
	}
	if root == nil {
		return nil, 0, nil, fmt.Errorf("request body must be a JSON object")
	}

	changed := 0
	renamed := make(map[string]string)
	restored := make(toolNameMapping)
	if tools, ok := root["tools"].([]any); ok {
		declaredNames := make(map[string]struct{}, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := tool["name"].(string); ok && name != "" {
				declaredNames[name] = struct{}{}
			}
		}
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// Custom tools may omit type or explicitly use "custom". Other typed
			// tools include server tools with fixed names; leave them unchanged,
			// including unknown types, for upstream validation.
			if toolType, typed := tool["type"]; typed && toolType != "custom" {
				continue
			}
			name, ok := tool["name"].(string)
			if !ok || name == "" || strings.HasPrefix(name, mcpToolNamePrefix) {
				continue
			}
			normalized := mcpToolNamePrefix + name
			if _, collision := declaredNames[normalized]; collision {
				return nil, 0, nil, fmt.Errorf("normalize tool name %q: target %q is already declared", name, normalized)
			}
			tool["name"] = normalized
			renamed[name] = normalized
			restored[normalized] = name
			changed++
		}
	}
	if toolChoice, ok := root["tool_choice"].(map[string]any); ok && renameToolReference(toolChoice, renamed) {
		changed++
	}
	if messages, ok := root["messages"].([]any); ok {
		for _, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				continue
			}
			content, ok := message["content"].([]any)
			if !ok {
				continue
			}
			for _, rawBlock := range content {
				block, ok := rawBlock.(map[string]any)
				if !ok || block["type"] != "tool_use" {
					continue
				}
				if renameToolReference(block, renamed) {
					changed++
				}
			}
		}
	}
	if changed == 0 {
		return body, 0, nil, nil
	}
	transformed, err := json.Marshal(root)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("encode tool-normalized request body: %w", err)
	}
	return transformed, changed, restored, nil
}

func renameToolReference(value map[string]any, renamed map[string]string) bool {
	name, ok := value["name"].(string)
	if !ok {
		return false
	}
	normalized, ok := renamed[name]
	if !ok {
		return false
	}
	value["name"] = normalized
	return true
}
