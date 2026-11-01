package audio

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/media"
)

var (
	ffmpegOnce sync.Once
	ffmpegPath string
	ffprobe    string
)

func lookupFFmpeg() {
	ffmpegOnce.Do(func() {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = p
		}
		if p, err := exec.LookPath("ffprobe"); err == nil {
			ffprobe = p
		}
	})
}

// FFmpegAvailable reports whether the optional FFmpeg backend is usable.
func FFmpegAvailable() bool {
	lookupFFmpeg()
	return ffmpegPath != ""
}

// DisableFFmpeg turns off the FFmpeg backend (used by --no-ffmpeg and tests).
func DisableFFmpeg() {
	lookupFFmpeg()
	ffmpegPath = ""
}

func init() {
	media.FFmpegAvailable = FFmpegAvailable
}

// ffmpegSource runs ffmpeg as a subprocess (no shell) and reads f32le stereo
// at the output rate from its stdout. FFmpeg handles AAC priming/padding
// (iTunSMPB / edit lists) itself.
type ffmpegSource struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	out    *bufio.Reader
	stderr *strings.Builder
	rate   int
	total  int64
	raw    []byte
	done   bool
}

type limitedBuilder struct {
	strings.Builder
}

func (l *limitedBuilder) Write(p []byte) (int, error) {
	if l.Len() < 4096 {
		l.Builder.Write(p)
	}
	return len(p), nil
}

func openFFmpeg(path string, outRate int) (Source, error) {
	if !FFmpegAvailable() {
		return nil, apperr.New(apperr.UnsupportedFormat, "%s requires ffmpeg, which is not installed", filepath.Base(path))
	}
	ctx, cancel := context.WithCancel(context.Background())
	// "-i" is followed by "file:" to stop ffmpeg from interpreting the path
	// as a protocol URL or option.
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", "file:"+path,
		"-vn", "-sn", "-dn",
		"-f", "f32le", "-acodec", "pcm_f32le",
		"-ac", "2", "-ar", strconv.Itoa(outRate),
		"pipe:1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, apperr.Wrap(apperr.DecodeError, err, "start ffmpeg")
	}
	lb := &limitedBuilder{}
	cmd.Stderr = lb
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, apperr.Wrap(apperr.DecodeError, err, "start ffmpeg")
	}
	s := &ffmpegSource{
		cmd:    cmd,
		cancel: cancel,
		out:    bufio.NewReaderSize(stdout, 64<<10),
		stderr: &lb.Builder,
		rate:   outRate,
		total:  probeDuration(path, outRate),
	}
	return s, nil
}

func probeDuration(path string, rate int) int64 {
	if ffprobe == "" {
		return -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error",
		"-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1",
		"file:"+path).Output()
	if err != nil {
		return -1
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || sec <= 0 || math.IsNaN(sec) {
		return -1
	}
	return int64(sec * float64(rate))
}

func (s *ffmpegSource) SampleRate() int    { return s.rate }
func (s *ffmpegSource) TotalFrames() int64 { return s.total }

func (s *ffmpegSource) Read(dst []float32) (int, error) {
	if s.done {
		return 0, io.EOF
	}
	frames := len(dst) / Channels
	need := frames * 8
	if cap(s.raw) < need {
		s.raw = make([]byte, need)
	}
	raw := s.raw[:need]
	n, err := io.ReadFull(s.out, raw)
	got := n / 8
	for i := 0; i < got*2; i++ {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			s.done = true
			werr := s.cmd.Wait()
			if werr != nil && got == 0 {
				msg := strings.TrimSpace(s.stderr.String())
				if msg == "" {
					msg = werr.Error()
				}
				return 0, apperr.New(apperr.DecodeError, "ffmpeg: %s", msg)
			}
			if got > 0 {
				return got, nil
			}
			return 0, io.EOF
		}
		return got, err
	}
	return got, nil
}

func (s *ffmpegSource) Close() error {
	s.cancel()
	if !s.done {
		s.done = true
		_ = s.cmd.Wait()
	}
	return nil
}
