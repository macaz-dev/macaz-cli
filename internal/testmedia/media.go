package testmedia

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
)

// TwoColorPNG returns a deterministic vision fixture whose left half is red
// and right half is blue. The dimensions are intentionally large enough to
// survive provider-side image preprocessing.
func TwoColorPNG() ([]byte, error) {
	picture := image.NewRGBA(image.Rect(0, 0, 512, 256))
	for y := picture.Bounds().Min.Y; y < picture.Bounds().Max.Y; y++ {
		for x := picture.Bounds().Min.X; x < picture.Bounds().Max.X; x++ {
			pixel := color.RGBA{R: 255, A: 255}
			if x >= picture.Bounds().Dx()/2 {
				pixel = color.RGBA{B: 255, A: 255}
			}
			picture.SetRGBA(x, y, pixel)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

// TextPDF creates a small, valid PDF with one extractable line of text.
func TextPDF(text string) []byte {
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
