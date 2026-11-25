package govideo_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/webp"
)

// Lossy WebP fixtures. Go has no WebP encoder and many ffmpeg builds have
// none either (Homebrew's, for one), so tests that made these with the
// oracle were skipped on most machines. They are written out here instead:
// made once with libwebp's cwebp 1.6.0 from pictures drawn by this package's
// tests, and small enough to read as text.
const (
	// whiteWebP is 64x64 of opaque white (cwebp -q 95): a VP8 picture.
	whiteWebP = "UklGRjYAAABXRUJQVlA4ICoAAAAwAwCdASpAAEAAPhkMhUIhBEEABABhLSAAAgbqagCvELcgAP7/PVgAAAA="

	// logoWebP is 200x200 (cwebp -q 95 -alpha_q 100 -exact): an opaque
	// white square at 50..150 in a transparent margin that hides red, with
	// a strip of half-transparent white (alpha 128) above y = 30. A VP8
	// picture with an alpha plane.
	logoWebP = `
UklGRqYCAABXRUJQVlA4WAoAAAAQAAAAxwAAxwAAQUxQSCkAAAABFyAQSFKffI2ICAdFjSQpi/7Y5Cq9P0b0fwLmaA9bvvVy/f7b
bt5tAQBWUDggVgIAADAbAJ0BKsgAyAA+GQyFQaEE4QIEAGEsrd9PbgEXrEDXv/APwg4j7b/5b+Cf7Ff0/fAfsBiAXwD8Vf4B7AH4
AdQB/APyA4AH8A/gH8A9rP9AOkA4AD9APRV/gHWN/w+/qvYA/ECSANVo/APwo5wD8QPgAGSEWN1rG6w4ohF33wQiW++CES33wQil
+zvxSPk6LxUY/CegwptX/DJiBcV9eBHp0c81BEFfROH/4RFiEpPTd45Y3PY5HxcvgiKgXZ4Io4V9dW5mZf8fWG92z1ivk6LydF5O
i8nReTovJ0Xk6LydFwAA/v/5k2AfPvPv/9sZqyTldR4T71RwgiJwXw8VcP0gOEJeiy7zczVXxQUFU33fUoOPhPHN9edmS6N1EVOA
bh2ocTYP/Rp/7Kcv4p7WuKvVwgBB1r+qpaFAF2dahwLH1gdHqVsQ7CDJmRVAV4MYddWn42PyDKswULX+jNkK8H2vnkwsIIBLiDKN
GBxU8detnL2JQ0MgyaRdDNhmUqGKgH7+ebcX/9iGX9Edu3FTaKUpaCaL4cfMr76wXP5zeg3mYHW0lgkngP7qpatGFiHBjG5HL2V9
L+njxYRQV5NPA1Np2ocUt7TM2DlsVoMpKTIaUHS53RQ8vpZDgY5v1++jT/+ynGeiO1rqRtFPJZwkFqG/MqeCW7NPR3UIMhJFHKWI
cNtFSlJAODY3xtCOnpdPlrFwvhx9JjshsiwUbBsHmFot8yP0yIlvrB3H1dBhgZMeSVq0niPuL50JMSq6Tesp903AjPZPQ4DrBQrz
n4qUoyK9FpAAAAA=`
)

// writeWebP writes one of the fixtures above to dir and returns its path
// and the picture it decodes to.
func writeWebP(t *testing.T, dir, name, fixture string) (string, image.Image) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(fixture), ""))
	if err != nil {
		t.Fatal(err)
	}
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, img
}

// TestWebPFixtures checks that the fixtures are what the tests using them
// take them for: lossy pictures, which decode to limited-range samples, the
// second with the red still there under its transparent margin.
func TestWebPFixtures(t *testing.T) {
	dir := t.TempDir()
	_, white := writeWebP(t, dir, "white.webp", whiteWebP)
	if y, ok := white.(*image.YCbCr); !ok || y.Rect.Dx() != 64 || y.Rect.Dy() != 64 {
		t.Errorf("the white fixture decodes to %T %v, want a 64x64 *image.YCbCr", white, white.Bounds())
	} else if l := y.Y[y.YOffset(32, 32)]; l < 233 || l > 237 {
		t.Errorf("the white fixture's luma is %d, want limited-range white (235)", l)
	}
	_, logo := writeWebP(t, dir, "logo.webp", logoWebP)
	n, ok := logo.(*image.NYCbCrA)
	if !ok || n.Rect.Dx() != 200 || n.Rect.Dy() != 200 {
		t.Fatalf("the logo fixture decodes to %T %v, want a 200x200 *image.NYCbCrA", logo, logo.Bounds())
	}
	for _, c := range []struct {
		x, y  int
		alpha uint8
		what  string
	}{{100, 100, 255, "square"}, {100, 15, 128, "strip"}, {20, 100, 0, "margin"}, {180, 180, 0, "margin"}} {
		if a := n.A[n.AOffset(c.x, c.y)]; a != c.alpha {
			t.Errorf("the logo fixture's alpha in the %s at (%d,%d) is %d, want %d", c.what, c.x, c.y, a, c.alpha)
		}
	}
	// Red under the margin: Cr well above the middle, where white has 128.
	if cr := n.Cr[n.COffset(20, 100)]; cr < 200 {
		t.Errorf("the logo fixture's margin has Cr %d: the red it hides is gone", cr)
	}
}
