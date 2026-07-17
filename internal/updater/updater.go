package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultLatestURL = "https://github.com/macaz-dev/macaz-cli/releases/latest"
	defaultAPIURL    = "https://api.github.com/repos/macaz-dev/macaz-cli/releases/latest"
	maxReleaseJSON   = 1 << 20
	maxChecksums     = 1 << 20
	maxBinary        = 128 << 20
)

var ErrDevelopmentBuild = errors.New("self-update is unavailable for a development build")

type Release struct {
	Current   string
	Latest    string
	Available bool
}

type Result struct {
	Current string
	Latest  string
	Path    string
	Updated bool
}

type Options struct {
	CurrentVersion string
	HTTPClient     *http.Client
	LatestURL      string
	APIURL         string
	GOOS           string
	GOARCH         string
	AllowHTTP      bool
	ExecutablePath func() (string, error)
	VerifyBinary   func(context.Context, string, string) error
}

type Client struct {
	currentVersion string
	httpClient     *http.Client
	latestURL      string
	apiURL         string
	goos           string
	goarch         string
	allowHTTP      bool
	executablePath func() (string, error)
	verifyBinary   func(context.Context, string, string) error
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
	Size               int64  `json:"size"`
}

type semVersion struct {
	major int
	minor int
	patch int
}

func New(currentVersion string) *Client {
	return NewWithOptions(Options{CurrentVersion: currentVersion})
}

func NewWithOptions(options Options) *Client {
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	latestURL := options.LatestURL
	if latestURL == "" {
		latestURL = defaultLatestURL
	}
	apiURL := options.APIURL
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	goos := options.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := options.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	executablePath := options.ExecutablePath
	if executablePath == nil {
		executablePath = resolvedExecutablePath
	}
	verifyBinary := options.VerifyBinary
	if verifyBinary == nil {
		verifyBinary = verifyBinaryVersion
	}
	return &Client{
		currentVersion: options.CurrentVersion,
		httpClient:     httpClient,
		latestURL:      latestURL,
		apiURL:         apiURL,
		goos:           goos,
		goarch:         goarch,
		allowHTTP:      options.AllowHTTP,
		executablePath: executablePath,
		verifyBinary:   verifyBinary,
	}
}

func (c *Client) Check(ctx context.Context) (Release, error) {
	current, err := parseVersion(c.currentVersion)
	if err != nil {
		return Release{}, ErrDevelopmentBuild
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, c.latestURL, nil)
	if err != nil {
		return Release{}, fmt.Errorf("create update-check request: %w", err)
	}
	c.addHeaders(request)
	response, err := c.do(request)
	if err != nil {
		return Release{}, fmt.Errorf("check latest release: %w", err)
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return Release{}, fmt.Errorf("check latest release: GitHub returned %s", response.Status)
	}
	latestTag, err := tagFromReleaseURL(response.Request.URL)
	if err != nil {
		return Release{}, err
	}
	if !c.allowHTTP && !strings.HasPrefix(response.Request.URL.Path, "/macaz-dev/macaz-cli/releases/tag/") {
		return Release{}, fmt.Errorf("latest release redirected outside the macaz repository")
	}
	latest, err := parseVersion(latestTag)
	if err != nil {
		return Release{}, fmt.Errorf("latest release has invalid version %q", latestTag)
	}
	return Release{
		Current:   canonicalVersion(current),
		Latest:    canonicalVersion(latest),
		Available: compareVersions(latest, current) > 0,
	}, nil
}

func (c *Client) Update(ctx context.Context) (Result, error) {
	current, err := parseVersion(c.currentVersion)
	if err != nil {
		return Result{}, ErrDevelopmentBuild
	}
	release, err := c.fetchRelease(ctx)
	if err != nil {
		return Result{}, err
	}
	latest, err := parseVersion(release.TagName)
	if err != nil {
		return Result{}, fmt.Errorf("latest release has invalid version %q", release.TagName)
	}
	result := Result{Current: canonicalVersion(current), Latest: canonicalVersion(latest)}
	if compareVersions(latest, current) <= 0 {
		return result, nil
	}

	binaryName, err := assetName(c.goos, c.goarch)
	if err != nil {
		return Result{}, err
	}
	binaryAsset, err := findAsset(release.Assets, binaryName)
	if err != nil {
		return Result{}, err
	}
	checksumAsset, err := findAsset(release.Assets, "SHA256SUMS")
	if err != nil {
		return Result{}, err
	}
	if err := c.validateReleaseAsset(binaryAsset, release.TagName); err != nil {
		return Result{}, err
	}
	if err := c.validateReleaseAsset(checksumAsset, release.TagName); err != nil {
		return Result{}, err
	}
	checksumData, err := c.downloadBytes(ctx, checksumAsset, maxChecksums)
	if err != nil {
		return Result{}, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	if err := verifyBytesDigest(checksumData, checksumAsset); err != nil {
		return Result{}, err
	}
	expectedDigest, err := checksumFor(checksumData, binaryName)
	if err != nil {
		return Result{}, err
	}
	if binaryAsset.Digest != "" {
		apiDigest, ok := strings.CutPrefix(strings.ToLower(binaryAsset.Digest), "sha256:")
		if !ok || !validDigest(apiDigest) {
			return Result{}, fmt.Errorf("GitHub returned an unsupported digest for %s", binaryName)
		}
		if apiDigest != expectedDigest {
			return Result{}, fmt.Errorf("GitHub digest and SHA256SUMS disagree for %s", binaryName)
		}
	}

	executable, err := c.executablePath()
	if err != nil {
		return Result{}, fmt.Errorf("locate current executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return Result{}, fmt.Errorf("resolve current executable path: %w", err)
	}
	info, err := os.Stat(executable)
	if err != nil {
		return Result{}, fmt.Errorf("inspect current executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("current executable is not a regular file: %s", executable)
	}
	releaseLock, err := acquireUpdateLock(executable)
	if err != nil {
		return Result{}, err
	}
	defer releaseLock()
	pattern := ".macaz-update-*"
	if c.goos == "windows" {
		pattern += ".exe"
	}
	staged, err := os.CreateTemp(filepath.Dir(executable), pattern)
	if err != nil {
		return Result{}, fmt.Errorf("cannot stage update beside %s: %w", executable, err)
	}
	stagedPath := staged.Name()
	keepStaged := false
	defer func() {
		_ = staged.Close()
		if !keepStaged {
			_ = os.Remove(stagedPath)
		}
	}()

	actualDigest, err := c.downloadFile(ctx, binaryAsset, staged, maxBinary)
	if err != nil {
		return Result{}, fmt.Errorf("download %s: %w", binaryName, err)
	}
	if err := staged.Close(); err != nil {
		return Result{}, fmt.Errorf("finish staged update: %w", err)
	}
	if actualDigest != expectedDigest {
		return Result{}, fmt.Errorf("checksum mismatch for %s", binaryName)
	}
	mode := info.Mode().Perm() | 0o500
	if err := os.Chmod(stagedPath, mode); err != nil {
		return Result{}, fmt.Errorf("make staged update executable: %w", err)
	}
	if err := c.verifyBinary(ctx, stagedPath, result.Latest); err != nil {
		return Result{}, fmt.Errorf("verify staged update: %w", err)
	}
	if err := replaceExecutable(stagedPath, executable); err != nil {
		if _, statErr := os.Stat(executable); errors.Is(statErr, os.ErrNotExist) {
			keepStaged = true
			return Result{}, fmt.Errorf("%w; verified update remains at %s", err, stagedPath)
		}
		return Result{}, err
	}
	keepStaged = true
	result.Path = executable
	result.Updated = true
	return result, nil
}

func CleanupOldExecutable() error {
	executable, err := resolvedExecutablePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(executable); err != nil {
		return err
	}
	err = os.Remove(backupPath(executable))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (c *Client) fetchRelease(ctx context.Context) (githubRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL, nil)
	if err != nil {
		return githubRelease{}, fmt.Errorf("create release request: %w", err)
	}
	c.addHeaders(request)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := c.do(request)
	if err != nil {
		return githubRelease{}, fmt.Errorf("query latest GitHub release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("query latest GitHub release: GitHub returned %s", response.Status)
	}
	body, err := readBounded(response.Body, maxReleaseJSON)
	if err != nil {
		return githubRelease{}, fmt.Errorf("read latest GitHub release: %w", err)
	}
	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return githubRelease{}, fmt.Errorf("decode latest GitHub release: %w", err)
	}
	if release.TagName == "" {
		return githubRelease{}, errors.New("latest GitHub release did not contain a tag")
	}
	return release, nil
}

func (c *Client) downloadBytes(ctx context.Context, asset githubAsset, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return nil, err
	}
	c.addHeaders(request)
	response, err := c.do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s", response.Status)
	}
	return readBounded(response.Body, limit)
}

func (c *Client) downloadFile(ctx context.Context, asset githubAsset, destination io.Writer, limit int64) (string, error) {
	if asset.Size < 0 || asset.Size > limit {
		return "", fmt.Errorf("asset size %d exceeds the %d-byte limit", asset.Size, limit)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return "", err
	}
	c.addHeaders(request)
	response, err := c.do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub returned %s", response.Status)
	}
	if response.ContentLength > limit {
		return "", fmt.Errorf("asset size exceeds the %d-byte limit", limit)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", err
	}
	if written > limit {
		return "", fmt.Errorf("asset size exceeds the %d-byte limit", limit)
	}
	if asset.Size > 0 && written != asset.Size {
		return "", fmt.Errorf("downloaded asset size %d does not match GitHub metadata %d", written, asset.Size)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (c *Client) do(request *http.Request) (*http.Response, error) {
	if err := c.validateURL(request.URL); err != nil {
		return nil, err
	}
	client := *c.httpClient
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if err := c.validateURL(request.URL); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	return client.Do(request)
}

func (c *Client) validateURL(target *url.URL) error {
	if c.allowHTTP && (target.Scheme == "http" || target.Scheme == "https") {
		return nil
	}
	if target.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS update URL: %s", target.Redacted())
	}
	host := strings.ToLower(target.Hostname())
	if host == "github.com" || host == "api.github.com" || strings.HasSuffix(host, ".githubusercontent.com") {
		return nil
	}
	return fmt.Errorf("refusing untrusted update host %q", host)
}

func (c *Client) validateReleaseAsset(asset githubAsset, tag string) error {
	if c.allowHTTP {
		return nil
	}
	target, err := url.Parse(asset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("release asset %s has an invalid URL: %w", asset.Name, err)
	}
	expectedPath := "/macaz-dev/macaz-cli/releases/download/" + tag + "/" + asset.Name
	if target.Scheme != "https" || strings.ToLower(target.Hostname()) != "github.com" || target.Path != expectedPath || target.RawQuery != "" || target.Fragment != "" {
		return fmt.Errorf("release asset %s has an unexpected download URL", asset.Name)
	}
	return nil
}

func (c *Client) addHeaders(request *http.Request) {
	request.Header.Set("User-Agent", "macaz/"+c.currentVersion)
}

func replaceExecutable(stagedPath, executable string) error {
	backup := backupPath(executable)
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale update backup %s: %w", backup, err)
	}
	if err := os.Rename(executable, backup); err != nil {
		return fmt.Errorf("prepare executable replacement %s: %w", executable, err)
	}
	if err := os.Rename(stagedPath, executable); err != nil {
		rollbackErr := os.Rename(backup, executable)
		if rollbackErr != nil {
			return fmt.Errorf("install update: %w (rollback also failed: %v)", err, rollbackErr)
		}
		return fmt.Errorf("install update: %w", err)
	}
	_ = os.Remove(backup)
	return nil
}

func backupPath(executable string) string {
	return filepath.Join(filepath.Dir(executable), "."+filepath.Base(executable)+".macaz-update-backup")
}

func acquireUpdateLock(executable string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(executable), "."+filepath.Base(executable)+".macaz-update-lock")
	for attempt := 0; attempt < 2; attempt++ {
		lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(lock, "%d\n", os.Getpid())
			return func() {
				_ = lock.Close()
				_ = os.Remove(lockPath)
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create update lock beside %s: %w", executable, err)
		}
		info, statErr := os.Stat(lockPath)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect macaz update lock: %w", statErr)
		}
		if time.Since(info.ModTime()) <= 10*time.Minute {
			return nil, fmt.Errorf("another macaz update is already in progress")
		}
		if err := os.Remove(lockPath); err != nil {
			return nil, fmt.Errorf("remove stale macaz update lock: %w", err)
		}
	}
	return nil, fmt.Errorf("another macaz update is already in progress")
}

func resolvedExecutablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}
	return filepath.Abs(path)
}

func verifyBinaryVersion(ctx context.Context, path, expected string) error {
	command := exec.CommandContext(ctx, path, "version")
	command.Env = append(os.Environ(), "MACAZ_NO_UPDATE_CHECK=1")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run staged binary: %w: %s", err, strings.TrimSpace(string(output)))
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || fields[0] != "macaz" || fields[1] != expected {
		return fmt.Errorf("staged binary reported unexpected version: %q", strings.TrimSpace(string(output)))
	}
	return nil
}

func tagFromReleaseURL(target *url.URL) (string, error) {
	const marker = "/releases/tag/"
	index := strings.Index(target.EscapedPath(), marker)
	if index < 0 {
		return "", fmt.Errorf("latest release redirected to an unexpected URL: %s", target.Redacted())
	}
	tag, err := url.PathUnescape(strings.TrimPrefix(target.EscapedPath()[index:], marker))
	if err != nil || tag == "" || strings.Contains(tag, "/") {
		return "", fmt.Errorf("latest release URL contains an invalid tag")
	}
	return tag, nil
}

func assetName(goos, goarch string) (string, error) {
	if goos != "darwin" && goos != "linux" && goos != "windows" {
		return "", fmt.Errorf("self-update is unsupported on %s/%s", goos, goarch)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("self-update is unsupported on %s/%s", goos, goarch)
	}
	name := "macaz-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name, nil
}

func findAsset(assets []githubAsset, name string) (githubAsset, error) {
	var found githubAsset
	count := 0
	for _, asset := range assets {
		if asset.Name == name {
			found = asset
			count++
		}
	}
	if count != 1 || found.BrowserDownloadURL == "" {
		return githubAsset{}, fmt.Errorf("latest release must contain exactly one %s asset", name)
	}
	return found, nil
}

func checksumFor(data []byte, name string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	var digest string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		candidate := strings.ToLower(fields[0])
		if !validDigest(candidate) {
			return "", fmt.Errorf("SHA256SUMS contains an invalid digest for %s", name)
		}
		if digest != "" {
			return "", fmt.Errorf("SHA256SUMS contains duplicate entries for %s", name)
		}
		digest = candidate
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read SHA256SUMS: %w", err)
	}
	if digest == "" {
		return "", fmt.Errorf("SHA256SUMS does not contain %s", name)
	}
	return digest, nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func verifyBytesDigest(data []byte, asset githubAsset) error {
	if asset.Digest == "" {
		return nil
	}
	expected, ok := strings.CutPrefix(strings.ToLower(asset.Digest), "sha256:")
	if !ok || !validDigest(expected) {
		return fmt.Errorf("GitHub returned an unsupported digest for %s", asset.Name)
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != expected {
		return fmt.Errorf("GitHub digest mismatch for %s", asset.Name)
	}
	return nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds the %d-byte limit", limit)
	}
	return data, nil
}

func parseVersion(value string) (semVersion, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semVersion{}, errors.New("version must use MAJOR.MINOR.PATCH")
	}
	values := make([]int, 3)
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semVersion{}, errors.New("version contains an invalid number")
		}
		parsed, err := strconv.ParseUint(part, 10, 31)
		if err != nil {
			return semVersion{}, errors.New("version contains an invalid number")
		}
		values[index] = int(parsed)
	}
	return semVersion{major: values[0], minor: values[1], patch: values[2]}, nil
}

func canonicalVersion(version semVersion) string {
	return fmt.Sprintf("v%d.%d.%d", version.major, version.minor, version.patch)
}

func compareVersions(left, right semVersion) int {
	leftParts := [...]int{left.major, left.minor, left.patch}
	rightParts := [...]int{right.major, right.minor, right.patch}
	for index := range leftParts {
		if leftParts[index] < rightParts[index] {
			return -1
		}
		if leftParts[index] > rightParts[index] {
			return 1
		}
	}
	return 0
}
