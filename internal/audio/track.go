// Package audio implements decoding into the engine's fixed output format:
// interleaved stereo float32 at the configured output sample rate.
package audio

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/media"
)

// Channels is the fixed output channel count.
const Channels = 2

// Source is a native decoder: it yields interleaved stereo float32 frames at
// its own sample rate.
type Source interface {
	// Read fills dst with interleaved stereo frames and returns the number of
	// frames written. It returns io.EOF when exhausted.
	Read(dst []float32) (int, error)
	// SampleRate is the native sample rate.
	SampleRate() int
	// TotalFrames is the number of native frames, or -1 when unknown.
	TotalFrames() int64
	Close() error
}

// Track is an opened, trimmed, and resampled decoder ready for the engine.
type Track struct {
	Path        string
	Format      media.Format
	src         Source
	resampler   *Resampler
	totalFrames int64 // at output rate, -1 unknown
	pre         []float32
	preOff      int
	eof         bool
}

// Open opens path for decoding into outRate stereo float32.
func Open(path string, outRate int) (*Track, error) {
	if err := media.CheckAudioFile(path); err != nil {
		return nil, err
	}
	fmtx, _ := media.FormatOf(path)
	src, err := openSource(path, fmtx, outRate)
	if err != nil {
		return nil, err
	}
	t := &Track{Path: path, Format: fmtx, src: src, totalFrames: -1}
	if n := src.TotalFrames(); n >= 0 {
		if src.SampleRate() == outRate {
			t.totalFrames = n
		} else {
			t.totalFrames = n * int64(outRate) / int64(src.SampleRate())
		}
	}
	if src.SampleRate() != outRate {
		t.resampler = NewResampler(src, outRate)
	}
	return t, nil
}

func openSource(path string, f media.Format, outRate int) (Source, error) {
	switch f {
	case media.FFmpeg:
		return openFFmpeg(path, outRate)
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apperr.New(apperr.FileNotFound, "%s does not exist", path)
		}
		return nil, apperr.Wrap(apperr.FileNotFound, err, "open %s", filepath.Base(path))
	}
	var src Source
	switch f {
	case media.WAV:
		src, err = newWAV(file)
	case media.MP3:
		src, err = newMP3(file)
	case media.OGG:
		src, err = newVorbis(file)
	case media.FLAC:
		src, err = newFLAC(file)
	default:
		err = apperr.New(apperr.UnsupportedFormat, "no decoder for %s", filepath.Base(path))
	}
	if err != nil {
		file.Close()
		if apperr.CodeOf(err) == apperr.Internal {
			return nil, apperr.Wrap(apperr.DecodeError, err, "decode %s", filepath.Base(path))
		}
		return nil, err
	}
	return src, nil
}

// TotalFrames returns the duration in output frames, or -1.
func (t *Track) TotalFrames() int64 { return t.totalFrames }

// Read fills dst (interleaved stereo at the output rate).
func (t *Track) Read(dst []float32) (int, error) {
	if t.preOff < len(t.pre) {
		n := copy(dst, t.pre[t.preOff:])
		t.preOff += n
		if t.preOff >= len(t.pre) {
			t.pre = nil
			t.preOff = 0
		}
		return n / Channels, nil
	}
	if t.eof {
		return 0, io.EOF
	}
	var n int
	var err error
	if t.resampler != nil {
		n, err = t.resampler.Read(dst)
	} else {
		n, err = t.src.Read(dst)
	}
	if errors.Is(err, io.EOF) {
		t.eof = true
		if n > 0 {
			return n, nil
		}
	}
	return n, err
}

// Prebuffer decodes up to frames output frames ahead, so the first Read after
// a gapless splice never waits on the decoder, and decode errors surface
// before the splice.
func (t *Track) Prebuffer(frames int) error {
	buf := make([]float32, frames*Channels)
	got := 0
	for got < frames {
		n, err := t.readRaw(buf[got*Channels:])
		got += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.eof = true
				break
			}
			return apperr.Wrap(apperr.DecodeError, err, "decode %s", filepath.Base(t.Path))
		}
		if n == 0 {
			break
		}
	}
	if got == 0 && t.eof {
		return apperr.New(apperr.DecodeError, "%s contains no audio", filepath.Base(t.Path))
	}
	t.pre = buf[:got*Channels]
	t.preOff = 0
	return nil
}

func (t *Track) readRaw(dst []float32) (int, error) {
	if t.resampler != nil {
		return t.resampler.Read(dst)
	}
	return t.src.Read(dst)
}

// Skip discards frames output frames (used to restore a saved position).
func (t *Track) Skip(frames int64) error {
	buf := make([]float32, 4096*Channels)
	for frames > 0 {
		want := int64(len(buf) / Channels)
		if frames < want {
			want = frames
		}
		n, err := t.Read(buf[:want*Channels])
		frames -= int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return nil
}

// Close releases the decoder.
func (t *Track) Close() error {
	if t == nil || t.src == nil {
		return nil
	}
	err := t.src.Close()
	t.src = nil
	return err
}

// toStereo converts one native frame with ch channels at index i of src
// (interleaved) into dst[0], dst[1].
func toStereo(ch int, l, r float32) (float32, float32) {
	if ch == 1 {
		return l, l
	}
	return l, r
}
