package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigBytesCodexRepeatedToolLoopThreshold(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    int
	}{
		{name: "default disabled", payload: `{}`, want: 0},
		{name: "configured", payload: "codex:\n  repeated-tool-loop-threshold: 3\n", want: 3},
		{name: "negative normalized", payload: "codex:\n  repeated-tool-loop-threshold: -2\n", want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(test.payload))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.Codex.RepeatedToolLoopThreshold; got != test.want {
				t.Fatalf("RepeatedToolLoopThreshold = %d, want %d", got, test.want)
			}
		})
	}
}

func TestLoadConfigNormalizesNegativeCodexRepeatedToolLoopThreshold(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("codex:\n  repeated-tool-loop-threshold: -1\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	cfg, errLoad := LoadConfig(configPath)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	if got := cfg.Codex.RepeatedToolLoopThreshold; got != 0 {
		t.Fatalf("RepeatedToolLoopThreshold = %d, want 0", got)
	}
}
