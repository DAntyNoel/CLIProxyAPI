package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

const responsesLiteToolLoopTestHeader = "X-OpenAI-Internal-Codex-Responses-Lite"

func TestResponsesLiteToolLoopGuardAppliesAtHTTPBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			executor := &responsesMultiAgentCaptureExecutor{}
			handler, modelID := newResponsesMultiAgentTestHandler(t, executor)
			handler.Cfg.CodexRepeatedToolLoopThreshold = 3
			router := gin.New()
			router.POST("/v1/responses", handler.Responses)

			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(responsesToolLoopTestPayload(modelID, stream, 3)))
			request.Header.Set(responsesLiteToolLoopTestHeader, "true")
			request.Header.Set("User-Agent", "codex_vscode/0.150.0-alpha.8")
			request.Header.Set("Originator", "codex_vscode")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}

			payloads := executor.Payloads()
			if len(payloads) != 1 {
				t.Fatalf("captured payload count = %d, want 1", len(payloads))
			}
			assertResponsesToolLoopRecovery(t, payloads[0])
		})
	}
}

func TestResponsesLiteToolLoopGuardWebsocketFallbackIsRequestLocal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &websocketDirectCaptureExecutor{provider: "codex"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "responses-loop-guard-ws-auth", Provider: "codex", Status: coreauth.StatusActive, ProxyURL: "direct"}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register auth: %v", errRegister)
	}
	modelID := "responses-loop-guard-ws-model"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelID}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{CodexRepeatedToolLoopThreshold: 3}, manager)
	handler := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	headers := make(http.Header)
	headers.Set(responsesLiteToolLoopTestHeader, "true")
	headers.Set("User-Agent", "codex_vscode/0.150.0-alpha.8")
	headers.Set("Originator", "codex_vscode")
	conn, _, errDial := websocket.DefaultDialer.Dial(wsURL, headers)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	first := appendResponseCreateType(responsesToolLoopTestPayload(modelID, true, 3))
	if errWrite := conn.WriteMessage(websocket.TextMessage, first); errWrite != nil {
		t.Fatalf("write first websocket request: %v", errWrite)
	}
	if _, _, errRead := conn.ReadMessage(); errRead != nil {
		t.Fatalf("read first websocket response: %v", errRead)
	}

	second := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"stream":true,"tool_choice":"auto","input":[{"role":"user","content":"new turn"}]}`, modelID))
	if errWrite := conn.WriteMessage(websocket.TextMessage, second); errWrite != nil {
		t.Fatalf("write second websocket request: %v", errWrite)
	}
	if _, _, errRead := conn.ReadMessage(); errRead != nil {
		t.Fatalf("read second websocket response: %v", errRead)
	}

	payloads := executor.Payloads()
	if len(payloads) != 2 {
		t.Fatalf("captured payload count = %d, want 2", len(payloads))
	}
	assertResponsesToolLoopRecovery(t, payloads[0])
	if names := loopGuardDeclaredToolNames(payloads[1]); !loopGuardContains(names, "collaboration__list_agents") {
		t.Fatalf("second websocket turn lost the request-local tool declaration: %v\n%s", names, payloads[1])
	}
}

func assertResponsesToolLoopRecovery(t *testing.T, payload []byte) {
	t.Helper()
	names := loopGuardDeclaredToolNames(payload)
	if loopGuardContains(names, "collaboration__list_agents") {
		t.Fatalf("repeating tool still declared: %v\n%s", names, payload)
	}
	for _, want := range []string{"functions__exec", "collaboration__send_message"} {
		if !loopGuardContains(names, want) {
			t.Fatalf("alternative %q missing from %v\n%s", want, names, payload)
		}
	}
	instructions := gjson.GetBytes(payload, "instructions").String()
	if !strings.Contains(instructions, "repeated no-progress tool-call loop") {
		t.Fatalf("recovery instructions missing: %q", instructions)
	}
	for _, forbidden := range []string{"list_agents", "path_prefix", "agent_status"} {
		if strings.Contains(instructions, forbidden) {
			t.Fatalf("recovery instructions leaked %q: %q", forbidden, instructions)
		}
	}
}

func responsesToolLoopTestPayload(modelID string, stream bool, rounds int) []byte {
	var input strings.Builder
	input.WriteString(`{"type":"additional_tools","role":"developer","tools":[`)
	input.WriteString(`{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]},`)
	input.WriteString(`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"list_agents"},{"type":"function","name":"send_message"}]}`)
	input.WriteString(`]},{"role":"user","content":"run the local check"}`)
	for round := 1; round <= rounds; round++ {
		fmt.Fprintf(&input, `,{"type":"function_call","call_id":"call-%d","namespace":"collaboration","name":"list_agents","arguments":"{\"path_prefix\":\"/root\"}"}`, round)
		fmt.Fprintf(&input, `,{"type":"function_call_output","call_id":"call-%d","output":"{\"agents\":[{\"agent_status\":\"running\"}]}"}`, round)
	}
	return []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"tool_choice":"auto","input":[%s]}`, modelID, stream, input.String()))
}

func appendResponseCreateType(payload []byte) []byte {
	return append([]byte(`{"type":"response.create",`), payload[1:]...)
}

func loopGuardDeclaredToolNames(payload []byte) []string {
	descriptors := util.CollectResponsesToolDescriptors(gjson.ParseBytes(payload))
	names := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		names = append(names, descriptor.Name)
	}
	return names
}

func loopGuardContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
