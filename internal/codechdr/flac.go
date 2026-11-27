package codechdr

// The channel assignments of a FLAC frame that holds two channels as
// something other than left and right (RFC 9639 §4.2, §9.1.3).
const (
	FLACLeftSide  = 8  // left, and left minus right
	FLACSideRight = 9  // left minus right, and right
	FLACMidSide   = 10 // their mean, and left minus right
)

// FLACMaxHeader is the most bytes a FLAC frame header takes: the sync code
// and the four fields behind it, a coded number of seven bytes, a block size
// and a sample rate of two bytes each, and the checksum.
const FLACMaxHeader = 16

// FLACFrame is what the header of a FLAC frame says about the frame
// (RFC 9639 §9.1).
type FLACFrame struct {
	// Variable is the blocking strategy bit: set, Number is the number of
	// the frame's first sample; clear, the stream has blocks of one size and
	// Number is the number of the frame.
	Variable  bool
	Number    uint64
	BlockSize int // sample frames in the frame, 1 to 65535
	// SampleRate and BitDepth are 0 where the header leaves them to the
	// STREAMINFO block.
	SampleRate int
	BitDepth   int
	Channels   int // 1 to 8
	// Assignment is the header's channel code: Channels-1 for channels
	// coded each on its own, else one of FLACLeftSide, FLACSideRight and
	// FLACMidSide.
	Assignment int
	Size       int // bytes in the header, its checksum included
}

// FLACFrameHeader decodes the header of a FLAC frame; ok is false where b
// does not start with a valid one, checksum included. A header takes at most
// FLACMaxHeader bytes.
func FLACFrameHeader(b []byte) (h FLACFrame, ok bool) {
	// The sync code is 15 bits; a reserved bit ends the fourth byte.
	if len(b) < 6 || b[0] != 0xff || b[1]&0xfe != 0xf8 || b[3]&1 != 0 {
		return h, false
	}
	h.Variable = b[1]&1 != 0
	sizeCode, rateCode := int(b[2]>>4), int(b[2]&15)
	assign, depthCode := int(b[3]>>4), int(b[3]>>1&7)
	if sizeCode == 0 || rateCode == 15 || assign > FLACMidSide || depthCode == 3 {
		return h, false
	}
	h.Assignment, h.Channels = assign, assign+1
	if assign >= FLACLeftSide {
		h.Channels = 2
	}
	h.BitDepth = [...]int{0, 8, 12, 0, 16, 20, 24, 32}[depthCode]

	// The coded number: UTF-8's scheme, carried on to seven bytes (§9.1.5).
	n := 0 // bytes after the first
	for n < 7 && b[4]&(0x80>>uint(n)) != 0 {
		n++
	}
	switch {
	case n == 0:
		h.Number = uint64(b[4])
	case n == 1 || n == 7 && b[4] != 0xfe:
		return h, false
	default:
		n--
		h.Number = uint64(b[4]) & (0x7f >> uint(n+1))
	}
	p := 5 + n
	if len(b) < p+1 {
		return h, false
	}
	for _, c := range b[5:p] {
		if c&0xc0 != 0x80 {
			return h, false
		}
		h.Number = h.Number<<6 | uint64(c&0x3f)
	}
	// A frame number has 31 bits at most.
	if !h.Variable && h.Number >= 1<<31 {
		return h, false
	}

	// read takes the next n bytes as a number, or fails the header.
	read := func(n int) int {
		if len(b) < p+n+1 {
			ok = false
			return 0
		}
		v := 0
		for _, c := range b[p : p+n] {
			v = v<<8 | int(c)
		}
		p += n
		return v
	}
	ok = true
	switch {
	case sizeCode == 1:
		h.BlockSize = 192
	case sizeCode <= 5:
		h.BlockSize = 576 << uint(sizeCode-2)
	case sizeCode == 6:
		h.BlockSize = read(1) + 1
	case sizeCode == 7:
		h.BlockSize = read(2) + 1
	default:
		h.BlockSize = 256 << uint(sizeCode-8)
	}
	switch rateCode {
	case 0:
	case 12:
		h.SampleRate = read(1) * 1000
	case 13:
		h.SampleRate = read(2)
	case 14:
		h.SampleRate = read(2) * 10
	default:
		h.SampleRate = [...]int{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}[rateCode]
	}
	// A block of 65536 samples cannot be stated in STREAMINFO and is
	// forbidden (§9.1.6), as is a rate of 0 written out (§9.1.7).
	if !ok || h.BlockSize > 65535 || rateCode != 0 && h.SampleRate == 0 {
		return h, false
	}
	if FLACCRC8(b[:p]) != b[p] {
		return h, false
	}
	h.Size = p + 1
	return h, true
}

var (
	flacCRC8Table  = flacMakeCRC8()
	flacCRC16Table = flacMakeCRC16()
)

func flacMakeCRC8() (t [256]uint8) {
	for i := range t {
		c := uint8(i)
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

// flacMakeCRC16 makes the tables of the frame checksum: t[k][v] is what a
// byte v leaves in the checksum after k more bytes, all zero, have gone
// through. With them eight bytes are taken in one step.
func flacMakeCRC16() (t [8][256]uint16) {
	for i := range t[0] {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
		t[0][i] = c
	}
	for k := 1; k < 8; k++ {
		for i, c := range t[k-1] {
			t[k][i] = c<<8 ^ t[0][c>>8]
		}
	}
	return t
}

// FLACCRC8 is the checksum of a FLAC frame header: polynomial
// x^8 + x^2 + x + 1, starting from 0 (RFC 9639 §9.1.8).
func FLACCRC8(b []byte) (c uint8) {
	for _, v := range b {
		c = flacCRC8Table[c^v]
	}
	return c
}

// FLACCRC16 carries the checksum of a FLAC frame on over b: polynomial
// x^16 + x^15 + x^2 + 1 (RFC 9639 §9.3). A frame's checksum starts from 0
// and covers everything before the two bytes that hold it.
func FLACCRC16(c uint16, b []byte) uint16 {
	t := &flacCRC16Table
	for ; len(b) >= 8; b = b[8:] {
		c ^= uint16(b[0])<<8 | uint16(b[1])
		c = t[7][c>>8] ^ t[6][c&0xff] ^ t[5][b[2]] ^ t[4][b[3]] ^ t[3][b[4]] ^ t[2][b[5]] ^ t[1][b[6]] ^ t[0][b[7]]
	}
	for _, v := range b {
		c = c<<8 ^ t[0][uint8(c>>8)^v]
	}
	return c
}
