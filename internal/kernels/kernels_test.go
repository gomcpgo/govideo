package kernels

import "testing"

func TestRGBToYUVExactEndpoints(t *testing.T) {
	for _, full := range []bool{false, true} {
		c := NewRGBToYUV(0.2126, 0.0722, full)
		white := []byte{255, 255, 255, 255}
		black := []byte{0, 0, 0, 255}
		var y, cb, cr [1]byte
		RGBAToYRow(y[:], white, 1, &c)
		RGBAToCbCrRow(cb[:], cr[:], white, white, 1, &c)
		wantW, wantB := byte(235), byte(16)
		if full {
			wantW, wantB = 255, 0
		}
		if y[0] != wantW || cb[0] != 128 || cr[0] != 128 {
			t.Errorf("full=%v white → %d,%d,%d", full, y[0], cb[0], cr[0])
		}
		RGBAToYRow(y[:], black, 1, &c)
		if y[0] != wantB {
			t.Errorf("full=%v black → %d", full, y[0])
		}
	}
}

func TestYUVConvIdentity(t *testing.T) {
	c := NewYUVConv(0.2126, 0.0722, false, 0.2126, 0.0722, false)
	y := []byte{16, 100, 235, 17}
	cb := []byte{90, 200}
	cr := []byte{240, 30}
	dy, dcb, dcr := make([]byte, 4), make([]byte, 2), make([]byte, 2)
	ConvertLumaRow(dy, y, cb, cr, 4, &c)
	ConvertChromaRow(dcb, dcr, cb, cr, 2, &c)
	for i := range y {
		if dy[i] != y[i] {
			t.Errorf("luma %d → %d", y[i], dy[i])
		}
	}
	for i := range cb {
		if dcb[i] != cb[i] || dcr[i] != cr[i] {
			t.Errorf("chroma %d,%d → %d,%d", cb[i], cr[i], dcb[i], dcr[i])
		}
	}
}

func TestResampleKernels(t *testing.T) {
	const one = 1 << FilterShift
	src := []byte{10, 20, 30, 40}
	dst := make([]byte, 2)
	ResampleH(dst, src, []int32{0, 2}, []int16{one / 2, one / 2, one / 2, one / 2}, 2)
	if dst[0] != 15 || dst[1] != 35 {
		t.Errorf("ResampleH: %v", dst)
	}
	// Two rows of width 2, stride 3.
	plane := []byte{10, 20, 0, 30, 40, 0}
	ResampleV(dst, plane, 3, 2, 0, []int16{one / 4, one * 3 / 4}, make([]int32, 2))
	if dst[0] != 25 || dst[1] != 35 {
		t.Errorf("ResampleV: %v", dst)
	}
	// Overshoot clips instead of wrapping.
	ResampleH(dst[:1], []byte{255, 0}, []int32{0}, []int16{one + 2000, -2000}, 2)
	if dst[0] != 255 {
		t.Errorf("clip high: %d", dst[0])
	}
	ResampleH(dst[:1], []byte{0, 255}, []int32{0}, []int16{one + 2000, -2000}, 2)
	if dst[0] != 0 {
		t.Errorf("clip low: %d", dst[0])
	}
}

func TestAudioKernels(t *testing.T) {
	x := []float32{1, 10, 2, 20, 3, 30}
	w := []float32{1, 2, 3}
	if got := DotInterleaved(x, w, 2, 0); got != 14 {
		t.Errorf("dot ch0 = %g", got)
	}
	if got := DotInterleaved(x, w, 2, 1); got != 140 {
		t.Errorf("dot ch1 = %g", got)
	}
	s := []float32{2, -4}
	Gain(s, 0.25)
	MixAdd(s, []float32{1, 1, 1})
	if s[0] != 1.5 || s[1] != 0 {
		t.Errorf("gain/mix: %v", s)
	}
}

func TestFieldMidRow(t *testing.T) {
	a := []byte{10, 0, 255, 0, 100}
	b := []byte{20, 0, 255, 255, 100}
	c := []byte{30, 0, 255, 255, 101}
	d := []byte{40, 255, 0, 0, 101}
	// (-a + 9b + 9c - d + 8) / 16: a ramp is continued, overshoot is
	// clipped, and halves round up.
	want := []byte{25, 0, 255, 255, 101}
	got := make([]byte, len(want))
	FieldMidRow(got, a, b, c, d)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d: %d, want %d", i, got[i], want[i])
		}
	}
}
