package codexcli

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/macaz-dev/macaz-cli/internal/config"
)

const maxGeneratedImageBytes = 32 << 20

func generatedImagePath(item map[string]any, threadID string) (string, error) {
	path := filepath.Clean(stringValue(item["savedPath"]))
	parent := filepath.Dir(path)
	// Codex app-server writes artifacts under generated_images/<thread-id>.
	// Never turn an unrelated path from a tool event into a file link.
	if filepath.IsAbs(path) && filepath.Base(parent) == threadID && filepath.Base(filepath.Dir(parent)) == "generated_images" {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".png", ".gif", ".jpg", ".jpeg", ".webp":
			if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
				return path, nil
			}
		}
	}

	encoded := strings.TrimSpace(stringValue(item["result"]))
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(maxGeneratedImageBytes) {
		return "", errors.New("Codex completed image generation without a readable image artifact")
	}
	image, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(image) > maxGeneratedImageBytes {
		return "", errors.New("Codex returned invalid image data")
	}
	var extension string
	switch http.DetectContentType(image) {
	case "image/png":
		extension = ".png"
	case "image/gif":
		extension = ".gif"
	case "image/jpeg":
		extension = ".jpg"
	case "image/webp":
		extension = ".webp"
	default:
		return "", errors.New("Codex returned an unsupported generated image format")
	}
	profile, err := config.ClaudeProfileDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(profile, "generated-images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create generated image directory: %w", err)
	}
	file, err := os.CreateTemp(dir, "image-*"+extension)
	if err != nil {
		return "", fmt.Errorf("create generated image: %w", err)
	}
	_, writeErr := file.Write(image)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("save generated image: %w", err)
	}
	return file.Name(), nil
}

func generatedImageLink(path string) string {
	location := url.URL{Path: filepath.ToSlash(path)}
	return "\n\n[Open generated image](<" + location.EscapedPath() + ">)\n\n"
}
