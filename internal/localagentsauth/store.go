package localagentsauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

var errCredentialChanged = errors.New("credential file changed while preparing the update")

type Source struct {
	Agent     string
	Provider  string
	Path      string
	Type      string
	Key       string
	Access    string
	Refresh   string
	Expires   int64
	AccountID string
	IDToken   string
}

func Scan() ([]Source, error) {
	paths, err := candidatePaths()
	if err != nil {
		return nil, err
	}
	var sources []Source
	var problems []error
	for agent, candidates := range paths {
		seen := map[string]bool{}
		for _, path := range candidates {
			path = filepath.Clean(path)
			if seen[path] {
				continue
			}
			seen[path] = true
			if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
				continue
			} else if statErr != nil {
				problems = append(problems, fmt.Errorf("inspect %s credentials at %s: %w", agent, path, statErr))
				continue
			}
			var found []Source
			err := WithLock(Source{Agent: agent, Path: path}, func() error {
				var scanErr error
				found, scanErr = scanFile(agent, path)
				return scanErr
			})
			if err != nil {
				problems = append(problems, fmt.Errorf("read %s credentials at %s: %w", agent, path, err))
			}
			sources = append(sources, found...)
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Agent != sources[j].Agent {
			return sources[i].Agent < sources[j].Agent
		}
		if sources[i].Provider != sources[j].Provider {
			return sources[i].Provider < sources[j].Provider
		}
		return sources[i].Path < sources[j].Path
	})
	return sources, errors.Join(problems...)
}

func ScanPath(path string) ([]Source, error) {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, path[2:])
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	if _, codexTokens := root["tokens"]; codexTokens || root["auth_mode"] != nil || root["OPENAI_API_KEY"] != nil {
		selected := Source{Agent: "codex", Provider: "openai", Path: path}
		var source Source
		err := WithLock(selected, func() error {
			current, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			source, readErr = decodeCodex(path, current)
			return readErr
		})
		return []Source{source}, err
	}
	agent := "opencode"
	for provider, entry := range root {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(entry, &kind) == nil && (kind.Type == "api_key" || provider == "openai-codex") {
			agent = "pi"
			break
		}
	}
	var sources []Source
	err = WithLock(Source{Agent: agent, Path: path}, func() error {
		var scanErr error
		sources, scanErr = scanFile(agent, path)
		return scanErr
	})
	return sources, err
}

func Get(agent, provider, path string) (Source, error) {
	path = filepath.Clean(path)
	if agent == "codex" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Source{}, err
		}
		return decodeCodex(path, raw)
	}
	entries, err := readEntries(path)
	if err != nil {
		return Source{}, fmt.Errorf("read %s credentials: %w", agent, err)
	}
	raw, ok := entries[provider]
	if !ok {
		return Source{}, fmt.Errorf("%s credential %q is not configured", agent, provider)
	}
	return decodeEntry(agent, provider, path, raw)
}

// WithLock uses proper-lockfile's auth.json.lock convention. Pi honors this
// lock directly; the heartbeat also prevents a long OAuth refresh going stale.
func WithLock(source Source, fn func() error) error {
	lockTarget := source.Path
	if resolved, err := filepath.EvalSymlinks(source.Path); err == nil {
		lockTarget = resolved
	}
	lockPath := lockTarget + ".lock"
	deadline := time.Now().Add(35 * time.Second)
	var owned os.FileInfo
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			owned, err = os.Stat(lockPath)
			if err != nil {
				return fmt.Errorf("inspect %s credential lock: %w", source.Agent, err)
			}
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("lock %s credentials: %w", source.Agent, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s credential lock", source.Agent)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now()
				_ = os.Chtimes(lockPath, now, now)
			case <-stop:
				return
			}
		}
	}()
	err := fn()
	close(stop)
	<-done
	current, statErr := os.Stat(lockPath)
	if statErr == nil && !os.SameFile(owned, current) {
		if err == nil {
			err = fmt.Errorf("%s credential lock ownership changed", source.Agent)
		}
		return err
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) && err == nil {
		return fmt.Errorf("inspect %s credential lock before release: %w", source.Agent, statErr)
	}
	if errors.Is(statErr, os.ErrNotExist) && err == nil {
		return fmt.Errorf("%s credential lock disappeared before release", source.Agent)
	}
	if statErr == nil {
		if removeErr := os.Remove(lockPath); err == nil && removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = fmt.Errorf("unlock %s credentials: %w", source.Agent, removeErr)
		}
	}
	return err
}

// UpdateOAuth must run under WithLock. expectedRefresh prevents a stale refresh
// from replacing a token rotated by an agent that does not honor file locks.
func UpdateOAuth(source Source, expectedRefresh, access, refresh string, expires int64, accountID, idToken string) (bool, error) {
	if source.Agent == "codex" {
		return updateCodexOAuth(source, expectedRefresh, access, refresh, accountID, idToken)
	}
	original, err := os.ReadFile(source.Path)
	if err != nil {
		return false, err
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(original, &entries); err != nil {
		return false, err
	}
	raw, ok := entries[source.Provider]
	if !ok {
		return false, fmt.Errorf("%s credential %q is not configured", source.Agent, source.Provider)
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false, err
	}
	if current, _ := entry["refresh"].(string); current != expectedRefresh {
		return false, nil
	}
	entry["access"] = access
	entry["refresh"] = refresh
	entry["expires"] = expires
	if accountID != "" {
		entry["accountId"] = accountID
	}
	updated, err := json.Marshal(entry)
	if err != nil {
		return false, err
	}
	entries[source.Provider] = updated
	if err := writeJSONIfUnchanged(source.Path, original, entries); err != nil {
		if errors.Is(err, errCredentialChanged) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func candidatePaths() (map[string][]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	result := map[string][]string{"opencode": {}, "pi": {}, "codex": {}}
	if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
		result["opencode"] = append(result["opencode"], filepath.Join(dataHome, "opencode", "auth.json"))
	}
	result["opencode"] = append(result["opencode"], filepath.Join(home, ".local", "share", "opencode", "auth.json"))
	if configHome, configErr := os.UserConfigDir(); configErr == nil {
		result["opencode"] = append(result["opencode"], filepath.Join(configHome, "opencode", "auth.json"))
	}
	if runtime.GOOS == "windows" {
		if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
			result["opencode"] = append(result["opencode"], filepath.Join(local, "opencode", "auth.json"))
		}
	}
	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
		result["pi"] = append(result["pi"], filepath.Join(dir, "auth.json"))
	}
	result["pi"] = append(result["pi"], filepath.Join(home, ".pi", "agent", "auth.json"))
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		result["codex"] = append(result["codex"], filepath.Join(dir, "auth.json"))
	}
	result["codex"] = append(result["codex"], filepath.Join(home, ".codex", "auth.json"))
	return result, nil
}

func scanFile(agent, path string) ([]Source, error) {
	if agent == "codex" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		source, err := decodeCodex(path, raw)
		if err != nil {
			return nil, err
		}
		return []Source{source}, nil
	}
	entries, err := readEntries(path)
	if err != nil {
		return nil, err
	}
	sources := make([]Source, 0, len(entries))
	var problems []error
	for provider, raw := range entries {
		source, err := decodeEntry(agent, provider, path, raw)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		sources = append(sources, source)
	}
	return sources, errors.Join(problems...)
}

func readEntries(path string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	return entries, nil
}

func decodeEntry(agent, provider, path string, raw json.RawMessage) (Source, error) {
	var value struct {
		Type      string `json:"type"`
		Key       string `json:"key"`
		Access    string `json:"access"`
		Refresh   string `json:"refresh"`
		Expires   int64  `json:"expires"`
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return Source{}, fmt.Errorf("decode %s credential %q: %w", agent, provider, err)
	}
	typeName := value.Type
	if typeName == "api_key" {
		typeName = "api"
	}
	if agent == "pi" && typeName == "api" && (strings.HasPrefix(value.Key, "!") || strings.Contains(value.Key, "$")) {
		typeName = "api_reference"
	}
	return Source{
		Agent: agent, Provider: provider, Path: path, Type: typeName, Key: value.Key,
		Access: value.Access, Refresh: value.Refresh, Expires: value.Expires, AccountID: value.AccountID,
	}, nil
}

func decodeCodex(path string, raw []byte) (Source, error) {
	var value struct {
		AuthMode string  `json:"auth_mode"`
		APIKey   *string `json:"OPENAI_API_KEY"`
		Tokens   *struct {
			Access    string `json:"access_token"`
			Refresh   string `json:"refresh_token"`
			AccountID string `json:"account_id"`
			IDToken   string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return Source{}, fmt.Errorf("decode Codex credentials: %w", err)
	}
	mode := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(value.AuthMode)))
	apiKey := value.APIKey != nil && strings.TrimSpace(*value.APIKey) != ""
	if apiKey && (mode == "apikey" || mode == "") {
		return Source{Agent: "codex", Provider: "openai", Path: path, Type: "api", Key: *value.APIKey}, nil
	}
	if value.Tokens == nil || value.Tokens.Access == "" || value.Tokens.Refresh == "" {
		if apiKey {
			return Source{Agent: "codex", Provider: "openai", Path: path, Type: "api", Key: *value.APIKey}, nil
		}
		return Source{}, errors.New("Codex auth.json contains no reusable OpenAI credential")
	}
	return Source{
		Agent: "codex", Provider: "openai", Path: path, Type: "oauth",
		Access: value.Tokens.Access, Refresh: value.Tokens.Refresh,
		Expires: jwtExpiresAt(value.Tokens.Access), AccountID: value.Tokens.AccountID, IDToken: value.Tokens.IDToken,
	}, nil
}

func updateCodexOAuth(source Source, expectedRefresh, access, refresh, accountID, idToken string) (bool, error) {
	raw, err := os.ReadFile(source.Path)
	if err != nil {
		return false, err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, err
	}
	tokens, ok := value["tokens"].(map[string]any)
	if !ok {
		return false, errors.New("Codex OAuth tokens are missing")
	}
	if current, _ := tokens["refresh_token"].(string); current != expectedRefresh {
		return false, nil
	}
	tokens["access_token"] = access
	tokens["refresh_token"] = refresh
	if accountID != "" {
		tokens["account_id"] = accountID
	}
	if idToken != "" {
		tokens["id_token"] = idToken
	}
	value["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeJSONIfUnchanged(source.Path, raw, value); err != nil {
		if errors.Is(err, errCredentialChanged) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func writeJSONIfUnchanged(path string, expected []byte, value any) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return errCredentialChanged
	}
	return writeJSON(path, value)
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceFile(tmpPath, target)
}

func jwtExpiresAt(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Expires int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	return claims.Expires * 1000
}
