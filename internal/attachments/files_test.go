package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/macaz-dev/macaz-cli/internal/protocol"
)

func TestMaterializeBase64UsesSafePrivateFile(t *testing.T) {
	dir := t.TempDir()
	paths, err := Materialize(context.Background(), dir, []protocol.Attachment{{
		Kind:     "document",
		Data:     base64.StdEncoding.EncodeToString([]byte("hello")),
		Filename: "../../unsafe name.txt",
	}}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Dir(paths[0]) != dir {
		t.Fatalf("paths = %#v", paths)
	}
	if filepath.Base(paths[0]) != "001-unsafe_name.txt" {
		t.Fatalf("path = %s", paths[0])
	}
	info, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("attachment mode = %o", info.Mode().Perm())
	}
}

func TestMaterializeRejectsOversizedBase64(t *testing.T) {
	_, err := Materialize(context.Background(), t.TempDir(), []protocol.Attachment{{
		Data:     base64.StdEncoding.EncodeToString([]byte("too large")),
		Filename: "file.txt",
	}}, 2)
	if err == nil {
		t.Fatal("expected size error")
	}
}

func TestTextReadsUTF8Documents(t *testing.T) {
	text, err := Text(context.Background(), protocol.Attachment{
		Kind:      "document",
		MediaType: "text/plain; charset=utf-8",
		Data:      base64.StdEncoding.EncodeToString([]byte("MACAZ_TEXT_OK\n")),
		Filename:  "notes.txt",
	}, 1024, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if text != "MACAZ_TEXT_OK\n" {
		t.Fatalf("text = %q", text)
	}
}

func TestTextExtractsPDFText(t *testing.T) {
	text, err := Text(context.Background(), protocol.Attachment{
		Kind:      "document",
		MediaType: "application/pdf",
		Data:      base64.StdEncoding.EncodeToString(testPDF("MACAZ_PDF_OK")),
		Filename:  "report.pdf",
	}, 1<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "MACAZ_PDF_OK") {
		t.Fatalf("text = %q", text)
	}
}

func TestTextRejectsUnsupportedBinaryDocument(t *testing.T) {
	_, err := Text(context.Background(), protocol.Attachment{
		Kind:      "document",
		MediaType: "application/zip",
		Data:      base64.StdEncoding.EncodeToString([]byte("zip")),
		Filename:  "archive.zip",
	}, 1024, 1024)
	if err == nil || !strings.Contains(err.Error(), "cannot safely expose") {
		t.Fatalf("err = %v", err)
	}
}

func testPDF(text string) []byte {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`(`, `\(`,
		`)`, `\)`,
	).Replace(text)
	stream := "BT\n/F1 12 Tf\n72 720 Td\n(" + escaped + ") Tj\nET\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n", len(objects)+1)
	output.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(
		&output,
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1,
		xref,
	)
	return output.Bytes()
}
