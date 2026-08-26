// Package toolloopguard provides a request-local circuit breaker for repeated
// no-progress tool calls in Codex Responses Lite transcripts.
package toolloopguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
)

const (
	responsesLiteHeader       = "X-OpenAI-Internal-Codex-Responses-Lite"
	responsesLiteMetadataPath = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"
	maxComparableValueBytes   = 1 << 20

	recoveryInstruction = "A repeated no-progress tool-call loop was detected. The repeating tool is intentionally unavailable for this response. Do not retry it or attempt an alias. Resume the user's pending plan with a different relevant available tool. If no available tool can make progress, explain the blocker instead of repeating another read-only call."
)

// Result describes a request rewrite without exposing tool arguments, output,
// or the tool name to callers that may log it.
type Result struct {
	Applied bool
	Repeats int
}

type toolIdentity struct {
	qualifiedName string
}

type pairSignature struct {
	callType      string
	qualifiedName string
	arguments     [sha256.Size]byte
	output        [sha256.Size]byte
}

// Apply removes a repeatedly selected tool from the current request and adds a
// fixed recovery instruction. It is deliberately stateless: the caller keeps
// the original request/session state and only executes the returned derivative.
func Apply(headers http.Header, payload []byte, repeatThreshold int) ([]byte, Result) {
	if repeatThreshold <= 0 || !gjson.ValidBytes(payload) {
		return payload, Result{}
	}

	root := gjson.ParseBytes(payload)
	if !isResponsesLiteRequest(headers, root) || !hasAutomaticToolChoice(root) {
		return payload, Result{}
	}

	target, repeats, repeated := repeatedTail(root.Get("input"), repeatThreshold)
	if !repeated {
		return payload, Result{}
	}

	object, ok := decodeJSONObject(payload)
	if !ok || !canAppendRecoveryInstruction(object) {
		return payload, Result{}
	}
	if !filterDeclaredTool(object, target) {
		return payload, Result{}
	}
	appendRecoveryInstruction(object)

	updated, err := json.Marshal(object)
	if err != nil {
		return payload, Result{}
	}
	return updated, Result{Applied: true, Repeats: repeats}
}

func isResponsesLiteRequest(headers http.Header, root gjson.Result) bool {
	if strings.EqualFold(strings.TrimSpace(headers.Get(responsesLiteHeader)), "true") {
		return true
	}
	value := root.Get(responsesLiteMetadataPath)
	return value.Type == gjson.True || value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.String()), "true")
}

func hasAutomaticToolChoice(root gjson.Result) bool {
	choice := root.Get("tool_choice")
	if choice.Type == gjson.String {
		return strings.EqualFold(strings.TrimSpace(choice.String()), "auto")
	}
	return choice.IsObject() && strings.EqualFold(strings.TrimSpace(choice.Get("type").String()), "auto")
}

func repeatedTail(input gjson.Result, repeatThreshold int) (toolIdentity, int, bool) {
	if !input.IsArray() {
		return toolIdentity{}, 0, false
	}
	items := input.Array()
	if repeatThreshold > len(items)/2 {
		return toolIdentity{}, 0, false
	}

	last, target, ok := completedPair(items[len(items)-2], items[len(items)-1])
	if !ok {
		return toolIdentity{}, 0, false
	}
	repeats := 1
	for i := len(items) - 4; i >= 0; i -= 2 {
		current, _, pairOK := completedPair(items[i], items[i+1])
		if !pairOK || current != last {
			break
		}
		repeats++
	}
	return target, repeats, repeats >= repeatThreshold
}

func completedPair(call, output gjson.Result) (pairSignature, toolIdentity, bool) {
	callType := strings.TrimSpace(call.Get("type").String())
	var outputType, argumentPath string
	switch callType {
	case "function_call":
		outputType = "function_call_output"
		argumentPath = "arguments"
	case "custom_tool_call":
		outputType = "custom_tool_call_output"
		argumentPath = "input"
	default:
		return pairSignature{}, toolIdentity{}, false
	}
	if strings.TrimSpace(output.Get("type").String()) != outputType {
		return pairSignature{}, toolIdentity{}, false
	}
	callID := call.Get("call_id").String()
	if callID == "" || output.Get("call_id").String() != callID {
		return pairSignature{}, toolIdentity{}, false
	}
	name := strings.TrimSpace(call.Get("name").String())
	if name == "" {
		return pairSignature{}, toolIdentity{}, false
	}
	arguments := call.Get(argumentPath)
	result := output.Get("output")
	if !arguments.Exists() || !result.Exists() {
		return pairSignature{}, toolIdentity{}, false
	}
	namespace := strings.TrimSpace(call.Get("namespace").String())
	qualifiedName := util.QualifyResponsesNamespaceToolName(namespace, name)
	argumentDigest, argumentsOK := canonicalValueDigest(arguments)
	outputDigest, outputOK := canonicalValueDigest(result)
	if !argumentsOK || !outputOK {
		return pairSignature{}, toolIdentity{}, false
	}
	signature := pairSignature{
		callType:      callType,
		qualifiedName: qualifiedName,
		arguments:     argumentDigest,
		output:        outputDigest,
	}
	return signature, toolIdentity{qualifiedName: qualifiedName}, true
}

func canonicalValueDigest(value gjson.Result) ([sha256.Size]byte, bool) {
	if len(value.Raw) > maxComparableValueBytes {
		return [sha256.Size]byte{}, false
	}
	raw := []byte(value.Raw)
	valueKind := byte('r')
	if value.Type == gjson.String {
		raw = []byte(value.String())
		valueKind = 's'
	}

	hash := sha256.New()
	_, _ = hash.Write([]byte{valueKind})
	if canonical, ok := canonicalJSON(raw); ok {
		_, _ = hash.Write([]byte{'j'})
		_, _ = hash.Write(canonical)
		var digest [sha256.Size]byte
		_ = hash.Sum(digest[:0])
		return digest, true
	}
	_, _ = hash.Write([]byte{'t'})
	_, _ = hash.Write(raw)
	var digest [sha256.Size]byte
	_ = hash.Sum(digest[:0])
	return digest, true
}

func canonicalJSON(raw []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, false
	}
	canonical, err := json.Marshal(value)
	return canonical, err == nil
}

func decodeJSONObject(payload []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, false
	}
	object, ok := value.(map[string]any)
	return object, ok
}

func canAppendRecoveryInstruction(root map[string]any) bool {
	value, exists := root["instructions"]
	if !exists || value == nil {
		return true
	}
	_, ok := value.(string)
	return ok
}

func appendRecoveryInstruction(root map[string]any) {
	existing, _ := root["instructions"].(string)
	if strings.Contains(existing, recoveryInstruction) {
		return
	}
	if strings.TrimSpace(existing) == "" {
		root["instructions"] = recoveryInstruction
		return
	}
	root["instructions"] = existing + "\n\n" + recoveryInstruction
}

func filterDeclaredTool(root map[string]any, target toolIdentity) bool {
	removed := false
	if tools, ok := root["tools"].([]any); ok {
		filtered, changed := filterToolList(tools, target, "")
		if changed {
			root["tools"] = filtered
			removed = true
		}
	}

	input, ok := root["input"].([]any)
	if !ok {
		return removed
	}
	for _, rawItem := range input {
		item, okItem := rawItem.(map[string]any)
		if !okItem || stringValue(item["type"]) != "additional_tools" {
			continue
		}
		tools, okTools := item["tools"].([]any)
		if !okTools {
			continue
		}
		filtered, changed := filterToolList(tools, target, "")
		if changed {
			item["tools"] = filtered
			removed = true
		}
	}
	return removed
}

func filterToolList(tools []any, target toolIdentity, parentNamespace string) ([]any, bool) {
	filtered := make([]any, 0, len(tools))
	changed := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			filtered = append(filtered, rawTool)
			continue
		}
		toolType := strings.TrimSpace(stringValue(tool["type"]))
		if toolType == "namespace" {
			namespaceName := strings.TrimSpace(stringValue(tool["name"]))
			children, hasChildren := tool["tools"].([]any)
			if hasChildren {
				updatedChildren, childChanged := filterToolList(children, target, namespaceName)
				if childChanged {
					changed = true
					if len(updatedChildren) == 0 {
						continue
					}
					tool["tools"] = updatedChildren
				}
			}
			filtered = append(filtered, tool)
			continue
		}

		name := declarationName(tool)
		qualifiedName := util.QualifyResponsesNamespaceToolName(parentNamespace, name)
		if name != "" && qualifiedName == target.qualifiedName {
			changed = true
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered, changed
}

func declarationName(tool map[string]any) string {
	if name := strings.TrimSpace(stringValue(tool["name"])); name != "" {
		return name
	}
	if function, ok := tool["function"].(map[string]any); ok {
		return strings.TrimSpace(stringValue(function["name"]))
	}
	return ""
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
