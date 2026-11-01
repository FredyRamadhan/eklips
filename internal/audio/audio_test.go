package audio

import (
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/frdn/eklips/internal/apperr"
)

func TestDecodeWAVFLACOGGMP3AndM4A(t *testing.T) {
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "tone.wav")
	const rate = 44100
	const frames = 44100 / 4 // 250 ms = 11025 frames
	if err := WriteSineWAV(wavPath, rate, frames, 440); err != nil {
		t.Fatal(err)
	}

	// 1. Native WAV
	tr, err := Open(wavPath, rate)
	if err != nil {
		t.Fatalf("Open WAV: %v", err)
	}
	gotWAV := readAllFrames(t, tr)
	_ = tr.Close()
	if gotWAV != frames {
		t.Fatalf("WAV frames = %d, want %d", gotWAV, frames)
	}

	// 2. Resampled WAV (22050 -> 44100)
	wav22 := filepath.Join(dir, "tone22.wav")
	if err := WriteSineWAV(wav22, 22050, 5512, 440); err != nil {
		t.Fatal(err)
	}
	tr22, err := Open(wav22, rate)
	if err != nil {
		t.Fatalf("Open 22k WAV: %v", err)
	}
	got22 := readAllFrames(t, tr22)
	_ = tr22.Close()
	if got22 < 10900 || got22 > 11100 {
		t.Fatalf("Resampled WAV frames = %d, want ~11024", got22)
	}

	if !FFmpegAvailable() {
		t.Skip("ffmpeg not installed; skipping converted codec tests")
	}

	// 3. Native FLAC
	flacPath := filepath.Join(dir, "tone.flac")
	if err := ConvertWithFFmpeg(wavPath, flacPath); err != nil {
		t.Fatalf("convert FLAC: %v", err)
	}
	trFLAC, err := Open(flacPath, rate)
	if err != nil {
		t.Fatalf("Open FLAC: %v", err)
	}
	gotFLAC := readAllFrames(t, trFLAC)
	_ = trFLAC.Close()
	if gotFLAC != frames {
		t.Fatalf("FLAC frames = %d, want %d", gotFLAC, frames)
	}

	// 4. Native OGG/Vorbis
	oggPath := filepath.Join(dir, "tone.ogg")
	if err := ConvertWithFFmpeg(wavPath, oggPath, "-c:a", "libvorbis"); err != nil {
		t.Fatalf("convert OGG: %v", err)
	}
	trOGG, err := Open(oggPath, rate)
	if err != nil {
		t.Fatalf("Open OGG: %v", err)
	}
	gotOGG := readAllFrames(t, trOGG)
	_ = trOGG.Close()
	if gotOGG <= 0 {
		t.Fatalf("OGG frames = %d, want > 0", gotOGG)
	}

	// 5. Native MP3 with LAME/Xing gapless metadata trimming
	mp3Path := filepath.Join(dir, "tone.mp3")
	if err := ConvertWithFFmpeg(wavPath, mp3Path, "-c:a", "libmp3lame", "-b:a", "192k"); err != nil {
		t.Fatalf("convert MP3: %v", err)
	}
	trMP3, err := Open(mp3Path, rate)
	if err != nil {
		t.Fatalf("Open MP3: %v", err)
	}
	gotMP3 := readAllFrames(t, trMP3)
	_ = trMP3.Close()
	// LAME tag should trim encoder delay and padding back to exact or near-exact frame count.
	if gotMP3 < frames-64 || gotMP3 > frames+64 {
		t.Fatalf("MP3 trimmed frames = %d, want ~%d", gotMP3, frames)
	}

	// 6. Optional FFmpeg backend for M4A/AAC
	m4aPath := filepath.Join(dir, "tone.m4a")
	if err := ConvertWithFFmpeg(wavPath, m4aPath, "-c:a", "aac", "-b:a", "128k"); err != nil {
		t.Fatalf("convert M4A: %v", err)
	}
	trM4A, err := Open(m4aPath, rate)
	if err != nil {
		t.Fatalf("Open M4A: %v", err)
	}
	gotM4A := readAllFrames(t, trM4A)
	_ = trM4A.Close()
	if gotM4A <= 0 {
		t.Fatalf("M4A frames = %d, want > 0", gotM4A)
	}
}

func TestUnsupportedFormatError(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "file.xyz")
	if err := WriteSineWAV(bad, 44100, 100, 440); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bad, 44100); !apperr.Is(err, apperr.UnsupportedFormat) {
		t.Fatalf("Open(.xyz): got %v, want UNSUPPORTED_FORMAT", err)
	}
}

func readAllFrames(t *testing.T, tr *Track) int {
	t.Helper()
	buf := make([]float32, 1024*Channels)
	total := 0
	for {
		n, err := tr.Read(buf)
		total += n
		if errors.Is(err, io.EOF) {
			return total
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
}

func BenchmarkDecodeWAV(b *testing.B) {
	dir := b.TempDir()
	wavPath := filepath.Join(dir, "bench.wav")
	// 1 second of 44.1 kHz stereo audio.
	if err := WriteSineWAV(wavPath, 44100, 44100, 440); err != nil {
		b.Fatal(err)
	}
	buf := make([]float32, 882*Channels)
	b.ResetTimer()
	b.SetBytes(44100 * 4)
	for i := 0; i < b.N; i++ {
		tr, err := Open(wavPath, 44100)
		if err != nil {
			b.Fatal(err)
		}
		for {
			_, err := tr.Read(buf)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
		_ = tr.Close()
	}
}

func BenchmarkResample48kTo44k(b *testing.B) {
	dir := b.TempDir()
	wavPath := filepath.Join(dir, "bench48.wav")
	if err := WriteSineWAV(wavPath, 48000, 48000, 440); err != nil {
		b.Fatal(err)
	}
	buf := make([]float32, 882*Channels)
	b.ResetTimer()
	b.SetBytes(44100 * 4)
	for i := 0; i < b.N; i++ {
		tr, err := Open(wavPath, 44100)
		if err != nil {
			b.Fatal(err)
		}
		for {
			_, err := tr.Read(buf)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
		_ = tr.Close()
	}
}
