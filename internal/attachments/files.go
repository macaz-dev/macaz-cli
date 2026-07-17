package attachments

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

const DefaultMaxBytes int64 = 10 << 20

var unsafeFilename = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

var downloadClient = &http.Client{
	Timeout: 60 * time.Second,
}

func Materialize(ctx context.Context, dir string, items []protocol.Attachment, maxBytes int64) ([]string, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	paths := make([]string, 0, len(items))
	for index, item := range items {
		name := safeFilename(item.Filename, index)
		path := filepath.Join(dir, fmt.Sprintf("%03d-%s", index+1, name))
		raw, err := Bytes(ctx, item, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("materialize attachment %q: %w", item.Filename, err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return nil, fmt.Errorf("write attachment %q: %w", item.Filename, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func Bytes(ctx context.Context, item protocol.Attachment, maxBytes int64) ([]byte, error) {
	if item.Data != "" {
		raw, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			return nil, fmt.Errorf("decode base64: %w", err)
		}
		if int64(len(raw)) > maxBytes {
			return nil, fmt.Errorf("attachment exceeds %d bytes", maxBytes)
		}
		return raw, nil
	}
	parsed, err := url.Parse(item.URL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download URL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("download URL returned HTTP %d", resp.StatusCode)
	}
	reader := io.LimitReader(resp.Body, maxBytes+1)
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("attachment exceeds %d bytes", maxBytes)
	}
	return raw, nil
}

func safeFilename(name string, index int) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = unsafeFilename.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("attachment-%d.bin", index+1)
	}
	if len(name) > 160 {
		extension := filepath.Ext(name)
		base := strings.TrimSuffix(name, extension)
		limit := 160 - len(extension)
		if limit < 1 {
			return name[:160]
		}
		name = base[:limit] + extension
	}
	return name
}

func DataURL(item protocol.Attachment) (string, error) {
	if item.Data == "" {
		if item.URL == "" {
			return "", errors.New("attachment has neither data nor URL")
		}
		return item.URL, nil
	}
	if _, err := base64.StdEncoding.DecodeString(item.Data); err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	mediaType := strings.TrimSpace(item.MediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + item.Data, nil
}
