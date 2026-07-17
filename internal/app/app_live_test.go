package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/config"
)

func TestLiveClaudeLifecycle(t *testing.T) {
	if os.Getenv("MACAZ_CLAUDE_INTEGRATION") != "1" {
		t.Skip("set MACAZ_CLAUDE_INTEGRATION=1 to run Claude Code through the configured live macaz provider")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.json")
	if err := config.SavePath(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACAZ_CONFIG", configPath)

	token := fmt.Sprintf("MACAZ_CLAUDE_TOOL_LOOP_%d", time.Now().UnixNano())
	inputPath := filepath.Join(root, "tool-loop.txt")
	if err := os.WriteFile(inputPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gatewayEnv := []string{
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_MODEL",
		"CLAUDE_CONFIG_DIR",
		"MACAZ_ACTIVE",
	}
	originalEnv := make(map[string]string, len(gatewayEnv))
	for _, key := range gatewayEnv {
		originalEnv[key] = os.Getenv(key)
	}

	prompt := fmt.Sprintf(
		"Use the Read tool to read %s. Reply with exactly the complete file contents and nothing else. Do not infer or guess the contents.",
		inputPath,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	err = Run(ctx, []string{
		"--dangerously-skip-permissions",
		"--allowedTools", "Read",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
		"-p",
		prompt,
	}, Streams{
		In:  strings.NewReader(""),
		Out: &stdout,
		Err: &stderr,
	})
	if err != nil {
		t.Fatalf("live Claude lifecycle: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), token) {
		t.Fatalf("Claude did not return the file token\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !streamContainsToolUse(stdout.Bytes(), "Read") {
		t.Fatalf("Claude did not expose a Read tool call in stream-json\nstdout:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "macaz: Claude Code") {
		t.Fatalf("missing macaz lifecycle banner in stderr:\n%s", stderr.String())
	}

	profileDir := filepath.Join(filepath.Dir(configPath), "claude")
	if _, err := os.Stat(filepath.Join(profileDir, "settings.json")); err != nil {
		t.Fatalf("isolated Claude profile was not created: %v", err)
	}
	for _, key := range gatewayEnv {
		if got := os.Getenv(key); got != originalEnv[key] {
			t.Fatalf("parent environment %s changed from %q to %q", key, originalEnv[key], got)
		}
	}
}

func streamContainsToolUse(raw []byte, toolName string) bool {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var value any
		if json.Unmarshal(scanner.Bytes(), &value) == nil && containsToolUse(value, toolName) {
			return true
		}
	}
	return false
}

func containsToolUse(value any, toolName string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if typed["type"] == "tool_use" && typed["name"] == toolName {
			return true
		}
		for _, child := range typed {
			if containsToolUse(child, toolName) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsToolUse(child, toolName) {
				return true
			}
		}
	}
	return false
}
