package audio

import (
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/frdn/eklips/internal/apperr"
	mp3 "github.com/hajimehoshi/go-mp3"
)

// mp3DecoderDelay is the inherent delay of the MP3 synthesis filterbank.
const mp3DecoderDelay = 529

// mp3Source decodes MP3 via go-mp3 (always 16-bit stereo output) and trims
// encoder delay and padding using the LAME/Xing tag when present.
type mp3Source struct {
	f        *os.File
	d        *mp3.Decoder
	rate     int
	raw      []byte
	skip     int64 // frames still to drop from the start
	limit    int64 // frames left to emit; -1 unlimited
	total    int64
	gapless  bool
	leftover int64
}

// lameInfo holds gapless metadata from a Xing/Info + LAME header.
type lameInfo struct {
	found   bool
	delay   int
	padding int
	// infoFrameSamples is the number of samples of the Info frame itself,
	// which a decoder renders as silence when it does not recognise it.
	infoFrameSamples int
}

func newMP3(f *os.File) (Source, error) {
	info := parseLAME(f)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	d, err := mp3.NewDecoder(f)
	if err != nil {
		return nil, apperr.Wrap(apperr.DecodeError, err, "invalid MP3")
	}
	s := &mp3Source{f: f, d: d, rate: d.SampleRate(), limit: -1, total: -1}
	decoded := int64(-1)
	if l := d.Length(); l > 0 {
		decoded = l / 4
	}
	if info.found {
		s.gapless = true
		s.skip = int64(info.infoFrameSamples + info.delay + mp3DecoderDelay)
		if decoded >= 0 {
			total := decoded - s.skip - int64(info.padding-mp3DecoderDelay)
			if info.padding < mp3DecoderDelay {
				total = decoded - s.skip
			}
			if total < 0 {
				total = 0
			}
			s.total = total
			s.limit = total
		}
	} else if decoded >= 0 {
		s.total = decoded
	}
	return s, nil
}

func (s *mp3Source) SampleRate() int    { return s.rate }
func (s *mp3Source) TotalFrames() int64 { return s.total }
func (s *mp3Source) Close() error       { return s.f.Close() }

func (s *mp3Source) Read(dst []float32) (int, error) {
	if s.limit == 0 {
		return 0, io.EOF
	}
	frames := len(dst) / Channels
	for {
		need := frames * 4
		if cap(s.raw) < need {
			s.raw = make([]byte, need)
		}
		raw := s.raw[:need]
		n, err := io.ReadFull(s.d, raw)
		got := n / 4
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, err
		}
		eof := err != nil
		start := 0
		if s.skip > 0 {
			drop := int64(got)
			if drop > s.skip {
				drop = s.skip
			}
			s.skip -= drop
			start = int(drop)
		}
		out := got - start
		if s.limit >= 0 && int64(out) > s.limit {
			out = int(s.limit)
		}
		for i := 0; i < out; i++ {
			b := raw[(start+i)*4:]
			dst[i*2] = float32(int16(binary.LittleEndian.Uint16(b))) / 32768
			dst[i*2+1] = float32(int16(binary.LittleEndian.Uint16(b[2:]))) / 32768
		}
		if s.limit >= 0 {
			s.limit -= int64(out)
		}
		if out > 0 {
			return out, nil
		}
		if eof || s.limit == 0 {
			return 0, io.EOF
		}
	}
}

// parseLAME reads the first MPEG frame after any ID3v2 tag and extracts the
// Xing/Info + LAME gapless fields.
func parseLAME(f *os.File) lameInfo {
	var info lameInfo
	buf := make([]byte, 10)
	if _, err := io.ReadFull(f, buf); err != nil {
		return info
	}
	off := int64(0)
	if string(buf[0:3]) == "ID3" {
		size := int64(buf[6]&0x7f)<<21 | int64(buf[7]&0x7f)<<14 | int64(buf[8]&0x7f)<<7 | int64(buf[9]&0x7f)
		off = 10 + size
		if buf[5]&0x10 != 0 {
			off += 10
		}
	}
	frame := make([]byte, 2048)
	n, _ := f.ReadAt(frame, off)
	frame = frame[:n]
	// Find frame sync within the first bytes (skip zero padding).
	i := 0
	for ; i+4 <= len(frame); i++ {
		if frame[i] == 0xFF && frame[i+1]&0xE0 == 0xE0 {
			break
		}
	}
	if i+4 > len(frame) {
		return info
	}
	frame = frame[i:]
	h := frame[1]
	version := (h >> 3) & 3 // 3 = MPEG1, 2 = MPEG2, 0 = MPEG2.5
	layer := (h >> 1) & 3   // 1 = Layer III
	if layer != 1 || version == 1 {
		return info
	}
	mono := (frame[3]>>6)&3 == 3
	var side int
	switch {
	case version == 3 && !mono:
		side = 32
	case version == 3 && mono:
		side = 17
	case !mono:
		side = 17
	default:
		side = 9
	}
	xo := 4 + side
	if len(frame) < xo+8 {
		return info
	}
	tag := string(frame[xo : xo+4])
	if tag != "Xing" && tag != "Info" {
		return info
	}
	flags := binary.BigEndian.Uint32(frame[xo+4:])
	p := xo + 8
	if flags&1 != 0 {
		p += 4
	}
	if flags&2 != 0 {
		p += 4
	}
	if flags&4 != 0 {
		p += 100
	}
	if flags&8 != 0 {
		p += 4
	}
	if version == 3 {
		info.infoFrameSamples = 1152
	} else {
		info.infoFrameSamples = 576
	}
	if len(frame) < p+24 {
		// Xing without LAME extension: only the Info frame is known.
		info.found = true
		return info
	}
	lame := frame[p:]
	enc := string(lame[0:4])
	if enc != "LAME" && enc != "Lavf" && enc != "Lavc" && enc != "GOGO" && enc != "L3.9" {
		info.found = true
		return info
	}
	d := lame[21:24]
	info.delay = int(d[0])<<4 | int(d[1])>>4
	info.padding = int(d[1]&0x0F)<<8 | int(d[2])
	info.found = true
	return info
}
