package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCheckReportsNewReleaseFromRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/releases/latest" {
			http.Redirect(response, request, "/macaz-dev/macaz-cli/releases/tag/v1.3.0", http.StatusFound)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := NewWithOptions(Options{
		CurrentVersion: "v1.2.3",
		HTTPClient:     server.Client(),
		LatestURL:      server.URL + "/releases/latest",
		AllowHTTP:      true,
	})
	release, err := client.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if release.Current != "v1.2.3" || release.Latest != "v1.3.0" || !release.Available {
		t.Fatalf("release = %#v", release)
	}
}

func TestDevelopmentBuildCheckDoesNotUseNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client := NewWithOptions(Options{
		CurrentVersion: "dev",
		HTTPClient:     server.Client(),
		LatestURL:      server.URL,
		AllowHTTP:      true,
	})
	_, err := client.Check(context.Background())
	if !errors.Is(err, ErrDevelopmentBuild) {
		t.Fatalf("error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("development check made %d requests", requests.Load())
	}
}

func TestUpdateVerifiesAndReplacesExecutable(t *testing.T) {
	payload := []byte("new macaz binary")
	digest := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(digest[:])
	target := filepath.Join(t.TempDir(), "macaz")
	if err := os.WriteFile(target, []byte("old macaz binary"), 0o751); err != nil {
		t.Fatal(err)
	}

	server := releaseServer(t, payload, digestHex, "sha256:"+digestHex)
	verified := false
	client := NewWithOptions(Options{
		CurrentVersion: "v1.2.3",
		HTTPClient:     server.Client(),
		APIURL:         server.URL + "/api/latest",
		GOOS:           "linux",
		GOARCH:         "amd64",
		AllowHTTP:      true,
		ExecutablePath: func() (string, error) { return target, nil },
		VerifyBinary: func(_ context.Context, path, expected string) error {
			verified = true
			if expected != "v1.2.4" {
				return fmt.Errorf("expected version = %s", expected)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(contents) != string(payload) {
				return fmt.Errorf("staged contents = %q", contents)
			}
			return nil
		},
	})
	result, err := client.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("staged binary was not verified")
	}
	if !result.Updated || result.Current != "v1.2.3" || result.Latest != "v1.2.4" || result.Path != target {
		t.Fatalf("result = %#v", result)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(payload) {
		t.Fatalf("installed contents = %q", contents)
	}
	if _, err := os.Stat(backupPath(target)); !os.IsNotExist(err) {
		t.Fatalf("backup still exists or stat failed: %v", err)
	}
	lockPath := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".macaz-update-lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("update lock still exists or stat failed: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o751 {
		t.Fatalf("installed mode = %o", info.Mode().Perm())
	}
}

func TestUpdateRejectsChecksumMismatchWithoutReplacingExecutable(t *testing.T) {
	payload := []byte("untrusted binary")
	wrongDigest := strings.Repeat("0", 64)
	target := filepath.Join(t.TempDir(), "macaz")
	oldPayload := []byte("trusted old binary")
	if err := os.WriteFile(target, oldPayload, 0o755); err != nil {
		t.Fatal(err)
	}

	server := releaseServer(t, payload, wrongDigest, "")
	client := NewWithOptions(Options{
		CurrentVersion: "v1.2.3",
		HTTPClient:     server.Client(),
		APIURL:         server.URL + "/api/latest",
		GOOS:           "linux",
		GOARCH:         "amd64",
		AllowHTTP:      true,
		ExecutablePath: func() (string, error) { return target, nil },
		VerifyBinary: func(context.Context, string, string) error {
			t.Fatal("checksum mismatch reached binary verification")
			return nil
		},
	})
	_, err := client.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v", err)
	}
	contents, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(contents) != string(oldPayload) {
		t.Fatalf("existing executable changed to %q", contents)
	}
}

func TestUpdateRejectsDisagreeingGitHubDigest(t *testing.T) {
	payload := []byte("new binary")
	digest := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(digest[:])
	server := releaseServer(t, payload, digestHex, "sha256:"+strings.Repeat("f", 64))
	client := NewWithOptions(Options{
		CurrentVersion: "v1.2.3",
		HTTPClient:     server.Client(),
		APIURL:         server.URL + "/api/latest",
		GOOS:           "linux",
		GOARCH:         "amd64",
		AllowHTTP:      true,
	})
	_, err := client.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpdateDoesNotDowngradeOrRewriteCurrentExecutable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(githubRelease{TagName: "v1.2.2"})
	}))
	t.Cleanup(server.Close)
	client := NewWithOptions(Options{
		CurrentVersion: "v1.2.3",
		HTTPClient:     server.Client(),
		APIURL:         server.URL,
		AllowHTTP:      true,
		ExecutablePath: func() (string, error) {
			t.Fatal("executable path requested for a downgrade")
			return "", nil
		},
	})
	result, err := client.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || result.Current != "v1.2.3" || result.Latest != "v1.2.2" {
		t.Fatalf("result = %#v", result)
	}
}

func TestReplaceExecutableRollsBackWhenInstallMoveFails(t *testing.T) {
	target := filepath.Join(t.TempDir(), "macaz")
	oldPayload := []byte("working binary")
	if err := os.WriteFile(target, oldPayload, 0o755); err != nil {
		t.Fatal(err)
	}
	err := replaceExecutable(filepath.Join(filepath.Dir(target), "missing-stage"), target)
	if err == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	contents, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(contents) != string(oldPayload) {
		t.Fatalf("rollback contents = %q", contents)
	}
	if _, statErr := os.Stat(backupPath(target)); !os.IsNotExist(statErr) {
		t.Fatalf("rollback backup still exists or stat failed: %v", statErr)
	}
}

func TestReleaseAssetURLIsRestrictedToMacazRepository(t *testing.T) {
	client := New("v1.2.3")
	asset := githubAsset{
		Name:               "macaz-linux-amd64",
		BrowserDownloadURL: "https://github.com/another/project/releases/download/v1.2.4/macaz-linux-amd64",
	}
	if err := client.validateReleaseAsset(asset, "v1.2.4"); err == nil {
		t.Fatal("asset from another repository was accepted")
	}
	asset.BrowserDownloadURL = "https://github.com/macaz-dev/macaz-cli/releases/download/v1.2.4/macaz-linux-amd64"
	if err := client.validateReleaseAsset(asset, "v1.2.4"); err != nil {
		t.Fatalf("official asset was rejected: %v", err)
	}
}

func TestVersionParsingAndComparison(t *testing.T) {
	for _, value := range []string{"dev", "v1", "v1.2", "v1.2.3-beta", "v01.2.3", "v1.-2.3"} {
		if _, err := parseVersion(value); err == nil {
			t.Fatalf("parseVersion(%q) succeeded", value)
		}
	}
	left, err := parseVersion("v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	right, err := parseVersion("1.99.99")
	if err != nil {
		t.Fatal(err)
	}
	if compareVersions(left, right) <= 0 || canonicalVersion(right) != "v1.99.99" {
		t.Fatalf("comparison failed: %#v %#v", left, right)
	}
}

func releaseServer(t *testing.T, payload []byte, checksumDigest, apiDigest string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/latest":
			release := githubRelease{
				TagName: "v1.2.4",
				Assets: []githubAsset{
					{
						Name:               "macaz-linux-amd64",
						BrowserDownloadURL: server.URL + "/asset/macaz-linux-amd64",
						Digest:             apiDigest,
						Size:               int64(len(payload)),
					},
					{
						Name:               "SHA256SUMS",
						BrowserDownloadURL: server.URL + "/asset/SHA256SUMS",
					},
				},
			}
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(release); err != nil {
				t.Error(err)
			}
		case "/asset/macaz-linux-amd64":
			_, _ = response.Write(payload)
		case "/asset/SHA256SUMS":
			_, _ = fmt.Fprintf(response, "%s  macaz-linux-amd64\n", checksumDigest)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
