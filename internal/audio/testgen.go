package audio

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
)

// WriteTestWAV writes a 16-bit stereo WAV file at path with the given sample
// rate and stereo int16 samples (len(samples) == frames*2).
func WriteTestWAV(path string, sampleRate int, samples []int16) error {
	dataBytes := len(samples) * 2
	buf := make([]byte, 44+dataBytes)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataBytes))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 2) // stereo
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sampleRate*4))
	binary.LittleEndian.PutUint16(buf[32:34], 4)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataBytes))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(s))
	}
	return os.WriteFile(path, buf, 0o644)
}

// WriteSineWAV writes a stereo sine-wave WAV file with the given frequency and
// frame count.
func WriteSineWAV(path string, sampleRate, frames int, freqHz float64) error {
	samples := make([]int16, frames*2)
	for i := 0; i < frames; i++ {
		v := int16(math.Sin(2*math.Pi*freqHz*float64(i)/float64(sampleRate)) * 16000)
		samples[i*2] = v
		samples[i*2+1] = v
	}
	return WriteTestWAV(path, sampleRate, samples)
}

// ConvertWithFFmpeg converts srcWAV to dstPath using the ffmpeg binary (used
// in tests for FLAC, OGG/Vorbis, MP3, and M4A/AAC).
func ConvertWithFFmpeg(srcWAV, dstPath string, extraArgs ...string) error {
	args := []string{"-y", "-nostdin", "-hide_banner", "-loglevel", "error", "-i", srcWAV}
	args = append(args, extraArgs...)
	args = append(args, dstPath)
	return exec.Command("ffmpeg", args...).Run()
}
