package toolloopguard

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
)

func TestApplyFiltersRepeatedNamespaceToolAndPreservesAlternatives(t *testing.T) {
	payload := repeatedFunctionPayload(3, `{"path_prefix":"/root"}`, `{"agents":[{"agent_name":"/root","agent_status":"running"}]}`)
	headers := liteHeaders()

	got, result := Apply(headers, payload, 3)
	if !result.Applied || result.Repeats != 3 {
		t.Fatalf("result = %+v, want applied with 3 repeats", result)
	}
	names := declaredToolNames(got)
	if contains(names, "collaboration__list_agents") {
		t.Fatalf("repeating tool still declared: %v", names)
	}
	for _, want := range []string{"functions__exec", "collaboration__send_message"} {
		if !contains(names, want) {
			t.Fatalf("alternative %q missing from %v", want, names)
		}
	}
	if gotInstructions := gjson.GetBytes(got, "instructions").String(); !strings.Contains(gotInstructions, recoveryInstruction) {
		t.Fatalf("instructions = %q, want recovery instruction", gotInstructions)
	} else if strings.Contains(gotInstructions, "list_agents") || strings.Contains(gotInstructions, "path_prefix") || strings.Contains(gotInstructions, "agent_status") {
		t.Fatalf("recovery instruction leaked request content: %q", gotInstructions)
	}
}

func TestApplyPreservesExistingInstructionsAndRemovesEveryDeclaration(t *testing.T) {
	payload := repeatedFunctionPayload(3, `{}`, `{"ok":true}`)
	payload = []byte(strings.Replace(string(payload), `"tool_choice":"auto",`, `"tool_choice":"auto","instructions":"keep this", "tools":[{"type":"function","name":"collaboration__list_agents"}],`, 1))

	headers := make(http.Header)
	headers.Set(responsesLiteHeader, "TRUE")
	got, result := Apply(headers, payload, 3)
	if !result.Applied {
		t.Fatalf("guard did not apply: %s", got)
	}
	if instructions := gjson.GetBytes(got, "instructions").String(); !strings.HasPrefix(instructions, "keep this\n\n") || !strings.Contains(instructions, recoveryInstruction) {
		t.Fatalf("instructions were not appended: %q", instructions)
	}
	if names := declaredToolNames(got); contains(names, "collaboration__list_agents") {
		t.Fatalf("duplicate declaration survived: %v", names)
	}
}

func TestApplyRemovesNamespaceWhenRepeatingToolWasItsOnlyChild(t *testing.T) {
	payload := repeatedFunctionPayload(3, `{}`, `{"ok":true}`)
	payload = bytes.Replace(payload,
		[]byte(`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"list_agents"},{"type":"function","name":"send_message"}]}`),
		[]byte(`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"list_agents"}]}`), 1)

	got, result := Apply(liteHeaders(), payload, 3)
	if !result.Applied {
		t.Fatalf("guard did not apply: %s", got)
	}
	for _, name := range declaredNamespaceNames(got) {
		if name == "collaboration" {
			t.Fatalf("empty collaboration namespace survived: %s", got)
		}
	}
	if !contains(declaredToolNames(got), "functions__exec") {
		t.Fatalf("unrelated namespace was removed: %s", got)
	}
}

func TestApplyCanonicalizesJSONArgumentsAndOutput(t *testing.T) {
	payload := []byte(`{"tool_choice":{"type":"auto"},"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true},"tools":[{"type":"custom","name":"poll"}],"input":[` +
		`{"type":"custom_tool_call","call_id":"1","name":"poll","input":"{ \"b\":2,\"a\":1 }"},{"type":"custom_tool_call_output","call_id":"1","output":"{\"ready\":false,\"n\":1}"},` +
		`{"type":"custom_tool_call","call_id":"2","name":"poll","input":"{\"a\":1,\"b\":2}"},{"type":"custom_tool_call_output","call_id":"2","output":"{ \"n\":1, \"ready\":false }"},` +
		`{"type":"custom_tool_call","call_id":"3","name":"poll","input":"{\n\"a\": 1, \"b\": 2}"},{"type":"custom_tool_call_output","call_id":"3","output":"{\"ready\":false,\"n\":1}"}]}`)

	got, result := Apply(nil, payload, 3)
	if !result.Applied {
		t.Fatalf("canonical JSON pairs did not trigger: %s", got)
	}
	if names := declaredToolNames(got); contains(names, "poll") {
		t.Fatalf("custom tool survived: %v", names)
	}
}

func TestApplySkipsRequestsOutsideStrictBoundary(t *testing.T) {
	base := repeatedFunctionPayload(3, `{}`, `{"ok":true}`)
	cases := []struct {
		name      string
		headers   http.Header
		payload   []byte
		threshold int
	}{
		{name: "disabled", headers: liteHeaders(), payload: base, threshold: 0},
		{name: "below threshold", headers: liteHeaders(), payload: repeatedFunctionPayload(2, `{}`, `{"ok":true}`), threshold: 3},
		{name: "non lite", payload: base, threshold: 3},
		{name: "required", headers: liteHeaders(), payload: bytes.Replace(base, []byte(`"tool_choice":"auto"`), []byte(`"tool_choice":"required"`), 1), threshold: 3},
		{name: "missing tool choice", headers: liteHeaders(), payload: bytes.Replace(base, []byte(`"tool_choice":"auto",`), nil, 1), threshold: 3},
		{name: "tool no longer declared", headers: liteHeaders(), payload: bytes.Replace(base, []byte(`{"type":"function","name":"list_agents"},`), nil, 1), threshold: 3},
		{name: "malformed", headers: liteHeaders(), payload: []byte(`{"tool_choice":"auto"`), threshold: 3},
		{name: "invalid instructions", headers: liteHeaders(), payload: bytes.Replace(base, []byte(`"tool_choice":"auto",`), []byte(`"tool_choice":"auto","instructions":[],`), 1), threshold: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, result := Apply(tc.headers, tc.payload, tc.threshold)
			if result.Applied {
				t.Fatalf("unexpected application: %+v", result)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Fatalf("payload changed\n got: %s\nwant: %s", got, tc.payload)
			}
		})
	}
}

func TestApplyStopsAtDifferentPairOrInterveningMessage(t *testing.T) {
	base := repeatedFunctionPayload(3, `{}`, `{"ok":true}`)
	changedOutput := bytes.Replace(base, []byte(`"output":"{\"ok\":true}"}`), []byte(`"output":"{\"ok\":false}"}`), 1)
	changedArguments := bytes.Replace(base, []byte(`"arguments":"{}"`), []byte(`"arguments":"{\"page\":2}"`), 1)
	withTrailingMessage := []byte(strings.TrimSuffix(string(base), `]}`) + `,{"role":"user","content":"continue"}]}`)
	parallelTail := bytes.Replace(base,
		[]byte(`{"type":"function_call","call_id":"call-3","namespace":"collaboration","name":"list_agents","arguments":"{}"},{"type":"function_call_output","call_id":"call-3","output":"{\"ok\":true}"}`),
		[]byte(`{"type":"function_call","call_id":"call-3","namespace":"collaboration","name":"list_agents","arguments":"{}"},{"type":"function_call","call_id":"other","namespace":"functions","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call-3","output":"{\"ok\":true}"},{"type":"function_call_output","call_id":"other","output":"ok"}`), 1)

	for name, payload := range map[string][]byte{
		"different output":      changedOutput,
		"different arguments":   changedArguments,
		"intervening user turn": withTrailingMessage,
		"parallel group":        parallelTail,
	} {
		t.Run(name, func(t *testing.T) {
			got, result := Apply(liteHeaders(), payload, 3)
			if result.Applied || !bytes.Equal(got, payload) {
				t.Fatalf("strict boundary was not preserved: %+v\n%s", result, got)
			}
		})
	}
}

func TestApplyHandlesLongIncidentShape(t *testing.T) {
	payload := repeatedFunctionPayload(80, `{"path_prefix":"/root"}`, `{"agents":[{"agent_name":"/root","agent_status":"running"}]}`)
	got, result := Apply(liteHeaders(), payload, 3)
	if !result.Applied || result.Repeats != 80 {
		t.Fatalf("result = %+v, want 80-repeat recovery", result)
	}
	if contains(declaredToolNames(got), "collaboration__list_agents") {
		t.Fatal("80-round incident tool survived")
	}
}

func TestApplySkipsOversizedToolValues(t *testing.T) {
	largeOutput := strings.Repeat("x", maxComparableValueBytes+1)
	payload := repeatedFunctionPayload(3, `{}`, largeOutput)
	got, result := Apply(liteHeaders(), payload, 3)
	if result.Applied || !bytes.Equal(got, payload) {
		t.Fatalf("oversized output should stay outside the bounded detector: %+v", result)
	}
}

func TestApplySkipsOverflowingThreshold(t *testing.T) {
	payload := repeatedFunctionPayload(1, `{}`, `{"ok":true}`)
	maxInt := int(^uint(0) >> 1)
	got, result := Apply(liteHeaders(), payload, maxInt)
	if result.Applied || !bytes.Equal(got, payload) {
		t.Fatalf("overflowing threshold should stay disabled for a short transcript: %+v", result)
	}
}

func repeatedFunctionPayload(rounds int, arguments, output string) []byte {
	var input strings.Builder
	input.WriteString(`{"type":"additional_tools","role":"developer","tools":[`)
	input.WriteString(`{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]},`)
	input.WriteString(`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"list_agents"},{"type":"function","name":"send_message"}]}`)
	input.WriteString(`]},{"role":"user","content":"run the next local check"}`)
	for round := 1; round <= rounds; round++ {
		fmt.Fprintf(&input, `,{"type":"function_call","call_id":"call-%d","namespace":"collaboration","name":"list_agents","arguments":%q}`, round, arguments)
		fmt.Fprintf(&input, `,{"type":"function_call_output","call_id":"call-%d","output":%q}`, round, output)
	}
	return []byte(`{"model":"gpt-test","tool_choice":"auto","input":[` + input.String() + `]}`)
}

func liteHeaders() http.Header {
	headers := make(http.Header)
	headers.Set(responsesLiteHeader, "true")
	return headers
}

func declaredToolNames(payload []byte) []string {
	descriptors := util.CollectResponsesToolDescriptors(gjson.ParseBytes(payload))
	names := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		names = append(names, descriptor.Name)
	}
	return names
}

func declaredNamespaceNames(payload []byte) []string {
	var names []string
	root := gjson.ParseBytes(payload)
	root.Get("input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "additional_tools" {
			return true
		}
		item.Get("tools").ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "namespace" {
				names = append(names, tool.Get("name").String())
			}
			return true
		})
		return true
	})
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
