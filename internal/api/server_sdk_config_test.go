package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestEffectiveSDKConfigCopiesCodexSettings(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{
		OptimizeMultiAgentV2:      true,
		RepeatedToolLoopThreshold: 3,
	}}

	sdkCfg := effectiveSDKConfig(cfg)
	if sdkCfg == nil || !sdkCfg.CodexOptimizeMultiAgentV2 {
		t.Fatalf("CodexOptimizeMultiAgentV2 = false, want true")
	}
	if got := sdkCfg.CodexRepeatedToolLoopThreshold; got != 3 {
		t.Fatalf("CodexRepeatedToolLoopThreshold = %d, want 3", got)
	}
}
