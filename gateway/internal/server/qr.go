package server

import (
	"bytes"
	"fmt"

	qrcode "github.com/skip2/go-qrcode"
)

// qrSVG renders one module per 1x1 path square. Server-side is deliberate: the
// dashboard ships no third-party assets (UI-NOTES), and the console is dark
// while a QR needs black-on-white, so the SVG is placed on a light card
// instead of being themed. Bitmap() already includes the quiet zone.
func qrSVG(content string) ([]byte, error) {
	if content == "" {
		return nil, fmt.Errorf("nothing to encode")
	}
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	bmp := q.Bitmap()
	n := len(bmp)
	if n == 0 || len(bmp[0]) != n {
		return nil, fmt.Errorf("unexpected qr bitmap shape")
	}
	var d bytes.Buffer
	d.Grow(n * n * 12)
	for y, row := range bmp {
		for x, on := range row {
			if !on {
				continue
			}
			fmt.Fprintf(&d, "M%d %dh1v1h-1z", x, y)
		}
	}
	var out bytes.Buffer
	fmt.Fprintf(&out,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="264" height="264" shape-rendering="crispEdges" role="img" aria-label="Pairing QR code">`,
		n, n)
	out.WriteString(`<rect width="100%" height="100%" fill="#fff"/><path fill="#000" d="`)
	out.Write(d.Bytes())
	out.WriteString(`"/></svg>`)
	return out.Bytes(), nil
}
