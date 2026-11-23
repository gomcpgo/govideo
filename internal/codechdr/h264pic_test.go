package codechdr

import (
	"encoding/binary"
	"testing"
)

// bitw writes the syntax elements of H.264 NAL units, so the tests can
// build streams no encoder at hand produces (field pictures above all).
type bitw struct {
	b []byte
	n int // bits written
}

func (w *bitw) u(n int, v uint32) {
	for i := n - 1; i >= 0; i-- {
		if w.n%8 == 0 {
			w.b = append(w.b, 0)
		}
		w.b[len(w.b)-1] |= byte(v>>uint(i)&1) << uint(7-w.n%8)
		w.n++
	}
}

func (w *bitw) flag(v bool) {
	if v {
		w.u(1, 1)
	} else {
		w.u(1, 0)
	}
}

func (w *bitw) ue(v uint32) {
	n := 0
	for (v+1)>>uint(n+1) != 0 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, v+1)
}

func (w *bitw) se(v int32) {
	if v > 0 {
		w.ue(uint32(2*v - 1))
	} else {
		w.ue(uint32(-2 * v))
	}
}

// nal ends the payload with the stop bit and returns it as a NAL unit with
// the given header, emulation prevention bytes inserted.
func (w *bitw) nal(header byte) []byte {
	w.u(1, 1)
	out := []byte{header}
	zeros := 0
	for _, c := range w.b {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// spsOpts describes a test sequence parameter set.
type spsOpts struct {
	id          uint32
	fields      bool // frame_mbs_only_flag = 0
	mbaff       bool
	pocType     uint32
	topToBottom int32 // offset_for_top_to_bottom_field (pocType 1)
	picStruct   bool  // pic_struct_present_flag
	hrd         bool  // NAL HRD parameters: picture timing starts with two delays
}

const (
	testCPBDelayBits = 7
	testDPBDelayBits = 5
)

func testSPS(o spsOpts) []byte {
	w := &bitw{}
	w.u(8, 77) // Main
	w.u(8, 0)
	w.u(8, 30)
	w.ue(o.id)
	w.ue(0) // log2_max_frame_num_minus4
	w.ue(o.pocType)
	switch o.pocType {
	case 0:
		w.ue(0) // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		w.flag(false) // delta_pic_order_always_zero_flag
		w.se(0)
		w.se(o.topToBottom)
		w.ue(0)
	}
	w.ue(1) // max_num_ref_frames
	w.flag(false)
	w.ue(19) // 320 wide
	w.ue(7)
	w.flag(!o.fields)
	if o.fields {
		w.flag(o.mbaff)
	}
	w.flag(true)  // direct_8x8_inference_flag
	w.flag(false) // frame_cropping_flag
	w.flag(true)  // vui_parameters_present_flag
	w.u(5, 0)     // aspect ratio, overscan, video signal, chroma location, timing: absent
	w.flag(o.hrd)
	if o.hrd {
		w.ue(0)
		w.u(8, 0)
		w.ue(100)
		w.ue(100)
		w.flag(false)
		w.u(5, 23)
		w.u(5, testCPBDelayBits-1)
		w.u(5, testDPBDelayBits-1)
		w.u(5, 24)
	}
	w.flag(false) // vcl_hrd_parameters_present_flag
	if o.hrd {
		w.flag(false) // low_delay_hrd_flag
	}
	w.flag(o.picStruct)
	w.flag(false) // bitstream_restriction_flag
	return w.nal(0x67)
}

func testPPS(id, sps uint32, bottomPOC bool) []byte {
	w := &bitw{}
	w.ue(id)
	w.ue(sps)
	w.flag(true) // entropy_coding_mode_flag
	w.flag(bottomPOC)
	w.ue(0) // num_slice_groups_minus1
	w.ue(0)
	w.ue(0)
	return w.nal(0x68)
}

// sliceOpts describes the header of a test slice; the SPS and PPS options
// repeat what the parameter sets it refers to were made with.
type sliceOpts struct {
	pps       uint32
	sps       spsOpts
	bottomPOC bool
	idr       bool
	field     bool
	bottom    bool
	delta     int32 // delta_pic_order_cnt_bottom, or delta_pic_order_cnt[1]
}

func testSlice(o sliceOpts) []byte {
	w := &bitw{}
	w.ue(0) // first_mb_in_slice
	w.ue(7) // slice_type
	w.ue(o.pps)
	w.u(4, 0) // frame_num
	if o.sps.fields {
		w.flag(o.field)
		if o.field {
			w.flag(o.bottom)
		}
	}
	if o.idr {
		w.ue(3)
	}
	switch o.sps.pocType {
	case 0:
		w.u(4, 2)
		if o.bottomPOC && !o.field {
			w.se(o.delta)
		}
	case 1:
		w.se(0)
		if o.bottomPOC && !o.field {
			w.se(o.delta)
		}
	}
	w.u(32, 0x12345678) // the rest of the header and the slice data: not read
	w.u(32, 0)
	w.u(32, 0x9abcdef0)
	if o.idr {
		return w.nal(0x65)
	}
	return w.nal(0x41)
}

// testSEI returns an SEI NAL unit with a picture timing message, preceded
// by a user data message when lead is set.
func testSEI(sps spsOpts, picStruct uint32, lead bool) []byte {
	w := &bitw{}
	if lead {
		w.u(8, 5)
		w.u(8, 20)
		for i := 0; i < 20; i++ {
			w.u(8, uint32(i))
		}
	}
	m := &bitw{}
	if sps.hrd {
		m.u(testCPBDelayBits, 0x55)
		m.u(testDPBDelayBits, 0x0a)
	}
	m.u(4, picStruct)
	m.u(4, 0)
	w.u(8, 1)
	w.u(8, uint32(len(m.b)))
	for _, c := range m.b {
		w.u(8, uint32(c))
	}
	return w.nal(0x06)
}

func testAVCC(sps []byte, pps ...[]byte) []byte {
	b := []byte{1, 77, 0, 30, 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	b = append(b, sps...)
	b = append(b, byte(len(pps)))
	for _, p := range pps {
		b = append(b, byte(len(p)>>8), byte(len(p)))
		b = append(b, p...)
	}
	return b
}

func testSample(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = binary.BigEndian.AppendUint32(b, uint32(len(n)))
		b = append(b, n...)
	}
	return b
}

func TestH264FieldOrder(t *testing.T) {
	prog := spsOpts{}
	mbaff := spsOpts{fields: true, mbaff: true}
	paff := spsOpts{fields: true}
	timed := spsOpts{fields: true, mbaff: true, picStruct: true}
	hrd := spsOpts{fields: true, mbaff: true, picStruct: true, hrd: true}
	poc1 := spsOpts{fields: true, mbaff: true, pocType: 1, topToBottom: -1}
	poc2 := spsOpts{fields: true, pocType: 2}

	frame := func(s spsOpts, bottomPOC bool, delta int32) []byte {
		return testSlice(sliceOpts{sps: s, bottomPOC: bottomPOC, delta: delta})
	}
	field := func(s spsOpts, bottom bool) []byte {
		return testSlice(sliceOpts{sps: s, field: true, bottom: bottom})
	}
	cases := []struct {
		name      string
		sps       spsOpts
		bottomPOC bool
		samples   [][]byte // the last one is the one checked
		want      FieldOrder
	}{
		{"frames only", prog, false, [][]byte{testSample(frame(prog, false, 0))}, Progressive},
		{"idr", mbaff, true, [][]byte{testSample(testSlice(sliceOpts{sps: mbaff, bottomPOC: true, idr: true, delta: -1}))}, BottomFieldFirst},
		{"mbaff, top count lower", mbaff, true, [][]byte{testSample(frame(mbaff, true, 1))}, TopFieldFirst},
		{"mbaff, bottom count lower", mbaff, true, [][]byte{testSample(frame(mbaff, true, -1))}, BottomFieldFirst},
		{"mbaff, counts equal", mbaff, true, [][]byte{testSample(frame(mbaff, true, 0))}, TopFieldFirst},
		{"mbaff, no counts", mbaff, false, [][]byte{testSample(frame(mbaff, false, 0))}, TopFieldFirst},
		{"count type 1", poc1, false, [][]byte{testSample(frame(poc1, false, 0))}, BottomFieldFirst},
		{"count type 1 with delta", poc1, true, [][]byte{testSample(frame(poc1, true, 3))}, TopFieldFirst},
		{"timing: frame", timed, true, [][]byte{testSample(testSEI(timed, PicStructFrame, false), frame(timed, true, 1))}, Progressive},
		{"timing: top, bottom", timed, true, [][]byte{testSample(testSEI(timed, PicStructTopBottom, false), frame(timed, true, -1))}, TopFieldFirst},
		{"timing: bottom, top", timed, true, [][]byte{testSample(testSEI(timed, PicStructBottomTop, false), frame(timed, true, 1))}, BottomFieldFirst},
		{"timing: three fields", timed, true, [][]byte{testSample(testSEI(timed, PicStructTopBottomTop, false), frame(timed, true, 1))}, Progressive},
		{"timing: doubled", timed, true, [][]byte{testSample(testSEI(timed, PicStructDoubling, false), frame(timed, true, 1))}, Progressive},
		{"timing after user data", timed, true, [][]byte{testSample(testSEI(timed, PicStructBottomTop, true), frame(timed, true, 1))}, BottomFieldFirst},
		{"timing after delays", hrd, true, [][]byte{testSample(testSEI(hrd, PicStructBottomTop, false), frame(hrd, true, 1))}, BottomFieldFirst},
		{"timing absent from the sample", timed, true, [][]byte{testSample(frame(timed, true, -1))}, BottomFieldFirst},
		{"field pair, top first", paff, false, [][]byte{testSample(field(paff, false), field(paff, false), field(paff, true))}, TopFieldFirst},
		{"field pair, bottom first", paff, false, [][]byte{testSample(field(paff, true), field(paff, false))}, BottomFieldFirst},
		{"one field", paff, false, [][]byte{testSample(field(paff, false))}, SingleField},
		{"one field in two slices", paff, false, [][]byte{testSample(field(paff, true), field(paff, true))}, SingleField},
		{"fields allowed, never used", paff, false, [][]byte{testSample(frame(paff, false, 0)), testSample(frame(paff, false, 0))}, Progressive},
		{"frame among field pictures", paff, false, [][]byte{testSample(field(paff, false), field(paff, true)), testSample(frame(paff, false, 0))}, TopFieldFirst},
		{"frame among field pictures, counts equal", poc2, false, [][]byte{testSample(field(poc2, false), field(poc2, true)), testSample(frame(poc2, false, 0))}, TopFieldFirst},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := NewH264Pictures(testAVCC(testSPS(c.sps), testPPS(0, 0, c.bottomPOC)))
			if err != nil {
				t.Fatal(err)
			}
			var pic H264Picture
			for _, s := range c.samples {
				pic = p.Picture(s)
			}
			if !pic.Found {
				t.Fatalf("no picture found: %+v", pic)
			}
			if got := pic.FieldOrder(); got != c.want {
				t.Errorf("field order %d, want %d (%+v)", got, c.want, pic)
			}
		})
	}
}

// TestH264ParameterSetsInBand covers parameter sets that arrive with the
// samples and replace the ones in the configuration record.
func TestH264ParameterSetsInBand(t *testing.T) {
	prog := spsOpts{}
	mbaff := spsOpts{fields: true, mbaff: true}
	p, err := NewH264Pictures(testAVCC(testSPS(prog), testPPS(0, 0, false)))
	if err != nil {
		t.Fatal(err)
	}
	if o := p.Picture(testSample(testSlice(sliceOpts{sps: prog}))).FieldOrder(); o != Progressive {
		t.Errorf("before the change: field order %d, want progressive", o)
	}
	s := testSample(testSPS(mbaff), testPPS(0, 0, true), testSlice(sliceOpts{sps: mbaff, bottomPOC: true, delta: -1}))
	if o := p.Picture(s).FieldOrder(); o != BottomFieldFirst {
		t.Errorf("after the change: field order %d, want bottom field first", o)
	}
	// A slice that names a picture parameter set never sent is not read.
	if pic := p.Picture(testSample(testSlice(sliceOpts{pps: 9, sps: mbaff}))); pic.Found || pic.FieldOrder() != Progressive {
		t.Errorf("unknown parameter set: %+v", pic)
	}
	// Two-byte NAL lengths.
	cfg := testAVCC(testSPS(mbaff), testPPS(0, 0, false))
	cfg[4] = 0xfd
	if p, err = NewH264Pictures(cfg); err != nil {
		t.Fatal(err)
	}
	nal := testSlice(sliceOpts{sps: mbaff})
	short := append([]byte{byte(len(nal) >> 8), byte(len(nal))}, nal...)
	if pic := p.Picture(short); !pic.Found || !pic.MBAFF {
		t.Errorf("two-byte lengths: %+v", pic)
	}
}

func TestH264PicturesHostile(t *testing.T) {
	mbaff := spsOpts{fields: true, mbaff: true, picStruct: true, hrd: true}
	cfg := testAVCC(testSPS(mbaff), testPPS(0, 0, true))
	sample := testSample(testSEI(mbaff, PicStructTopBottom, true), testSlice(sliceOpts{sps: mbaff, bottomPOC: true, delta: 1}))
	for n := 0; n <= len(cfg); n++ {
		if p, err := NewH264Pictures(cfg[:n]); err == nil {
			p.Picture(sample)
		}
	}
	p, err := NewH264Pictures(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n <= len(sample); n++ {
		p.Picture(sample[:n])
	}
	for i := range sample {
		for _, x := range []byte{0x01, 0x80, 0xff} {
			c := append([]byte(nil), sample...)
			c[i] ^= x
			p.Picture(c)
		}
	}
	// Lengths that run past the sample, and a sample of nothing but lengths.
	p.Picture([]byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3})
	p.Picture(make([]byte, 1<<16))
}

func FuzzH264Pictures(f *testing.F) {
	for _, s := range []spsOpts{{}, {fields: true, mbaff: true, picStruct: true, hrd: true}, {fields: true, pocType: 1, topToBottom: -1}} {
		cfg := testAVCC(testSPS(s), testPPS(0, 0, true))
		f.Add(cfg, testSample(testSEI(s, PicStructBottomTop, true), testSlice(sliceOpts{sps: s, bottomPOC: true, delta: 1})))
		f.Add(cfg, testSample(testSPS(s), testPPS(1, 0, false), testSlice(sliceOpts{pps: 1, sps: s, field: s.fields}), testSlice(sliceOpts{pps: 1, sps: s, field: s.fields, bottom: true})))
	}
	f.Add(unhex(f, seedAVCCInterlaced), []byte{0, 0, 0, 2, 0x65, 0x88})
	f.Fuzz(func(t *testing.T, cfg, sample []byte) {
		p, err := NewH264Pictures(cfg)
		if err != nil {
			return
		}
		pic := p.Picture(sample)
		if o := pic.FieldOrder(); !pic.Found && o != Progressive {
			t.Fatalf("field order %d for a sample with no picture", o)
		}
		p.Picture(sample) // parameter sets from the first pass are in place
	})
}
