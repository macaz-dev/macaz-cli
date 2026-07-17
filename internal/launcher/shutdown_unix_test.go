//go:build !windows

package launcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/config"
)

func TestClaudeGetsGracefulInterruptWhenContextExpires(t *testing.T) {
	root := t.TempDir()
	reportPath := filepath.Join(root, "ready.json")
	t.Setenv("MACAZ_CONFIG", filepath.Join(root, "macaz", "config.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "source-claude"))
	t.Setenv("MACAZ_FAKE_CLAUDE", "1")
	t.Setenv("MACAZ_FAKE_CLAUDE_WAIT", "1")
	t.Setenv("MACAZ_FAKE_CLAUDE_REPORT", reportPath)

	cfg := config.Default()
	cfg.ClaudeExecutable = os.Args[0]
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- Claude(ctx, cfg, Options{
			BaseURL:      "http://127.0.0.1:54321",
			Token:        "loopback-secret",
			Models:       []string{"claude-macaz-fake-a1b2c3d4"},
			DefaultModel: "claude-macaz-fake-a1b2c3d4",
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(reportPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("fake Claude did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-result:
	case <-time.After(claudeShutdownGracePeriod + 4*time.Second):
		t.Fatal("Claude did not exit after the graceful interrupt")
	}
	if _, err := os.Stat(reportPath + ".interrupted"); err != nil {
		t.Fatalf("Claude did not receive the graceful interrupt: %v", err)
	}
}
