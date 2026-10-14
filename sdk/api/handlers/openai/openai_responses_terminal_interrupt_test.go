package openai

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type completedHTTPInterruptExecutor struct {
	*blockingHTTPInterruptExecutor
	calls atomic.Int32
}

func (e *completedHTTPInterruptExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	id := fmt.Sprintf("r-http-%d", e.calls.Add(1))
	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id))}
	chunks <- coreexecutor.StreamChunk{Payload: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[]}}`, id))}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestResponsesTerminalHTTPInterruptDoesNotPoisonNextCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &completedHTTPInterruptExecutor{blockingHTTPInterruptExecutor: &blockingHTTPInterruptExecutor{}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{})
	manager.RegisterExecutor(executor)
	const authID = "terminal-http-interrupt-auth"
	const model = "terminal-http-interrupt-model"
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: authID, Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "test-key"},
	}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient(authID)
	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	send := func(payload string) {
		t.Helper()
		if err := client.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(event, id string) {
		t.Helper()
		_, payload, err := client.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(payload, "type").String(); got != event {
			t.Fatalf("event = %q, want %q: %s", got, event, payload)
		}
		if id != "" && gjson.GetBytes(payload, "response.id").String() != id {
			t.Fatalf("response ID = %q, want %q: %s", gjson.GetBytes(payload, "response.id").String(), id, payload)
		}
	}
	send(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, model))
	expect("response.created", "r-http-1")
	expect("response.completed", "r-http-1")
	// Let the handler finish the terminal turn before sending the control frame.
	time.Sleep(100 * time.Millisecond)
	send(`{"type":"response.interrupt","response_id":"unknown"}`)
	expect("error", "")
	send(`{"type":"response.interrupt","response_id":"r-http-1","mode":"discard_partial_items"}`)
	send(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, model))
	expect("response.created", "r-http-2")
	expect("response.completed", "r-http-2")
	if got := executor.calls.Load(); got != 2 {
		t.Fatalf("execution calls = %d, want 2", got)
	}
}
