package attachments

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	pdfreader "github.com/ledongthuc/pdf"
	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

const DefaultMaxTextBytes int64 = 2 << 20

// Text returns bounded, model-readable text for a document attachment. It is
// used by provider surfaces that do not expose a generic native file input.
func Text(ctx context.Context, item protocol.Attachment, maxInputBytes, maxTextBytes int64) (string, error) {
	if maxInputBytes <= 0 {
		maxInputBytes = DefaultMaxBytes
	}
	if maxTextBytes <= 0 {
		maxTextBytes = DefaultMaxTextBytes
	}
	raw, err := Bytes(ctx, item, maxInputBytes)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(item.MediaType, ";")[0]))
	switch {
	case strings.HasPrefix(mediaType, "text/"), isTextApplication(mediaType):
		if int64(len(raw)) > maxTextBytes {
			return "", fmt.Errorf("document text exceeds %d bytes", maxTextBytes)
		}
		if !utf8.Valid(raw) {
			return "", fmt.Errorf("document %q is not valid UTF-8", item.Filename)
		}
		return string(raw), nil
	case mediaType == "application/pdf", mediaType == "":
		return pdfText(ctx, raw, maxTextBytes)
	default:
		return "", fmt.Errorf(
			"local text fallback cannot safely expose document media type %q",
			item.MediaType,
		)
	}
}

func isTextApplication(mediaType string) bool {
	switch mediaType {
	case "application/json",
		"application/javascript",
		"application/toml",
		"application/xml",
		"application/x-httpd-php",
		"application/x-javascript",
		"application/x-ndjson",
		"application/x-sh",
		"application/x-yaml",
		"application/yaml":
		return true
	default:
		return false
	}
}

func pdfText(ctx context.Context, raw []byte, maxTextBytes int64) (text string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("extract PDF text: %v", recovered)
			text = ""
		}
	}()
	reader, err := pdfreader.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("open PDF: %w", err)
	}
	plain, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("extract PDF text: %w", err)
	}
	extracted, err := io.ReadAll(io.LimitReader(plain, maxTextBytes+1))
	if err != nil {
		return "", fmt.Errorf("read PDF text: %w", err)
	}
	if int64(len(extracted)) > maxTextBytes {
		return "", fmt.Errorf("extracted PDF text exceeds %d bytes", maxTextBytes)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !utf8.Valid(extracted) {
		return "", fmt.Errorf("extracted PDF text is not valid UTF-8")
	}
	text = strings.TrimSpace(string(extracted))
	if text == "" {
		return "", fmt.Errorf(
			"PDF contains no extractable text; this provider path cannot inspect image-only PDFs without native file input",
		)
	}
	return text, nil
}
