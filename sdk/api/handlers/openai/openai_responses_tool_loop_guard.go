package openai

import (
	"net/http"

	"github.com/gin-gonic/gin"
	toolloopguard "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/responses-lite-tool-loop-guard"
	log "github.com/sirupsen/logrus"
)

func (h *OpenAIResponsesAPIHandler) applyCodexResponsesLiteToolLoopGuard(c *gin.Context, payload []byte) []byte {
	if h == nil || h.Cfg == nil || h.Cfg.CodexRepeatedToolLoopThreshold <= 0 {
		return payload
	}
	var headers http.Header
	if c != nil && c.Request != nil {
		headers = c.Request.Header
	}
	updated, result := toolloopguard.Apply(headers, payload, h.Cfg.CodexRepeatedToolLoopThreshold)
	if result.Applied {
		log.Warnf(
			"responses lite repeated tool loop guard applied repeats=%d threshold=%d",
			result.Repeats,
			h.Cfg.CodexRepeatedToolLoopThreshold,
		)
	}
	return updated
}
