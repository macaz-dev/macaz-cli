package localagentsauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestScanFindsOpenCodePiAndCodex(t *testing.T) {
	home := t.TempDir()
	dataHome := filepath.Join(home, "data")
	piHome := filepath.Join(home, "pi")
	codexHome := filepath.Join(home, "codex")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("PI_CODING_AGENT_DIR", piHome)
	t.Setenv("CODEX_HOME", codexHome)
	writeTestFile(t, filepath.Join(dataHome, "opencode", "auth.json"), `{"openai":{"type":"api","key":"openai-key"}}`)
	writeTestFile(t, filepath.Join(piHome, "auth.json"), `{"openai-codex":{"type":"oauth","access":"pi-access","refresh":"pi-refresh","expires":1234,"accountId":"pi-account"}}`)
	writeTestFile(t, filepath.Join(codexHome, "auth.json"), `{"auth_mode":"chatgpt","OPENAI_API_KEY":"inactive-key","tokens":{"access_token":"codex-access","refresh_token":"codex-refresh","account_id":"codex-account"}}`)

	sources, err := Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 3 {
		t.Fatalf("sources = %#v", sources)
	}
	if sources[0].Agent != "codex" || sources[0].Provider != "openai" || sources[0].Type != "oauth" ||
		sources[1].Agent != "opencode" || sources[1].Type != "api" ||
		sources[2].Agent != "pi" || sources[2].Provider != "openai-codex" {
		t.Fatalf("sources = %#v", sources)
	}
}

func TestOAuthUpdateUsesLockPreservesDataAndRejectsStaleToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeTestFile(t, path, `{"openai-codex":{"type":"oauth","access":"old","refresh":"expected","expires":1,"accountId":"acct","custom":"keep"}}`)
	source := Source{Agent: "pi", Provider: "openai-codex", Path: path}
	if err := WithLock(source, func() error {
		if _, err := os.Stat(path + ".lock"); err != nil {
			t.Fatalf("compatible lock is missing: %v", err)
		}
		updated, err := UpdateOAuth(source, "expected", "new-access", "new-refresh", 1234, "acct-next", "")
		if err != nil {
			return err
		}
		if !updated {
			t.Fatal("fresh credential was not updated")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || !contains(raw, `"refresh": "new-refresh"`) || !contains(raw, `"custom": "keep"`) {
		t.Fatalf("updated auth = %s", raw)
	}
	if err := WithLock(source, func() error {
		updated, err := UpdateOAuth(source, "expected", "stale", "stale", 2, "", "")
		if updated {
			t.Fatal("stale refresh overwrote the current credential")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexOAuthUpdatePreservesTokenPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeTestFile(t, path, `{"auth_mode":"chatgpt","tokens":{"id_token":"id-token","access_token":"old","refresh_token":"expected","account_id":"acct"},"custom":"keep"}`)
	source := Source{Agent: "codex", Provider: "openai", Path: path}
	if err := WithLock(source, func() error {
		updated, err := UpdateOAuth(source, "expected", "new-access", "new-refresh", 1234, "acct-next", "new-id-token")
		if !updated {
			t.Fatal("Codex credential was not updated")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"id_token": "new-id-token"`, `"refresh_token": "new-refresh"`, `"custom": "keep"`, `"last_refresh"`} {
		if !contains(raw, value) {
			t.Fatalf("Codex auth is missing %s: %s", value, raw)
		}
	}
}

func TestPiSymlinkUsesCanonicalLockAndPreservesLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires optional Windows privileges")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real-auth.json")
	link := filepath.Join(dir, "auth.json")
	writeTestFile(t, target, `{"openai-codex":{"type":"oauth","access":"old","refresh":"expected","expires":1,"accountId":"acct"}}`)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	source := Source{Agent: "pi", Provider: "openai-codex", Path: link}
	if err := WithLock(source, func() error {
		if _, err := os.Stat(target + ".lock"); err != nil {
			t.Fatalf("canonical Pi lock is missing: %v", err)
		}
		updated, err := UpdateOAuth(source, "expected", "new", "next", 2, "acct", "")
		if !updated {
			t.Fatal("symlinked credential was not updated")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("credential update replaced the symlink")
	}
}

func TestPiCommandAPIKeyIsNotDirectlyReusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeTestFile(t, path, `{"openai":{"type":"api_key","key":"!security find-generic-password"}}`)
	sources, err := ScanPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Type != "api_reference" {
		t.Fatalf("sources = %#v", sources)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(raw []byte, value string) bool {
	for index := 0; index+len(value) <= len(raw); index++ {
		if string(raw[index:index+len(value)]) == value {
			return true
		}
	}
	return false
}
