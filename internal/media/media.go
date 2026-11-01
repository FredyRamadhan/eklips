// Package media handles audio file detection and folder scanning.
package media

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/frdn/eklips/internal/apperr"
)

// Format is a container/codec family.
type Format string

// Known formats.
const (
	WAV    Format = "wav"
	MP3    Format = "mp3"
	OGG    Format = "ogg"
	FLAC   Format = "flac"
	FFmpeg Format = "ffmpeg" // decoded through the optional FFmpeg backend
)

var extFormats = map[string]Format{
	".wav":  WAV,
	".wave": WAV,
	".mp3":  MP3,
	".ogg":  OGG,
	".oga":  OGG,
	".flac": FLAC,
	".m4a":  FFmpeg,
	".aac":  FFmpeg,
	".mp4":  FFmpeg,
	".opus": FFmpeg,
	".wma":  FFmpeg,
	".aif":  FFmpeg,
	".aiff": FFmpeg,
	".alac": FFmpeg,
	".webm": FFmpeg,
	".mka":  FFmpeg,
}

// FFmpegAvailable reports whether the FFmpeg backend may be used. It is set by
// the audio package at init time.
var FFmpegAvailable = func() bool { return false }

// FormatOf returns the format for a file path based on its extension.
func FormatOf(path string) (Format, bool) {
	f, ok := extFormats[strings.ToLower(filepath.Ext(path))]
	return f, ok
}

// IsAudio reports whether path has a playable audio extension, taking FFmpeg
// availability into account.
func IsAudio(path string) bool {
	f, ok := FormatOf(path)
	if !ok {
		return false
	}
	if f == FFmpeg {
		return FFmpegAvailable()
	}
	return true
}

// IsPlaylist reports whether path looks like a JSON playlist.
func IsPlaylist(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".json")
}

// CheckAudioFile validates that path exists, is a regular file, and has a
// supported audio extension.
func CheckAudioFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return apperr.New(apperr.FileNotFound, "%s does not exist", path)
		}
		return apperr.Wrap(apperr.FileNotFound, err, "cannot access %s", path)
	}
	if !st.Mode().IsRegular() {
		return apperr.New(apperr.UnsupportedFormat, "%s is not a regular file", path)
	}
	f, ok := FormatOf(path)
	if !ok {
		return apperr.New(apperr.UnsupportedFormat, "%s is not a supported audio format", filepath.Base(path))
	}
	if f == FFmpeg && !FFmpegAvailable() {
		return apperr.New(apperr.UnsupportedFormat, "%s requires ffmpeg, which is not installed", filepath.Base(path))
	}
	return nil
}

// Scan returns the supported audio files directly inside dir, in natural
// sort order. Hidden files are skipped. Subdirectories are not traversed.
func Scan(dir string, limit int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apperr.New(apperr.FileNotFound, "%s does not exist", dir)
		}
		return nil, apperr.Wrap(apperr.FileNotFound, err, "cannot read %s", dir)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		if !e.Type().IsRegular() {
			// Follow symlinks to regular files.
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			st, err := os.Stat(full)
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
		}
		if IsAudio(name) {
			out = append(out, full)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return NaturalLess(filepath.Base(out[i]), filepath.Base(out[j]))
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// NaturalLess compares strings so that embedded numbers sort numerically
// ("track2" < "track10") and letters compare case-insensitively.
func NaturalLess(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		ca, cb := ra[i], rb[j]
		if unicode.IsDigit(ca) && unicode.IsDigit(cb) {
			si := i
			for i < len(ra) && unicode.IsDigit(ra[i]) {
				i++
			}
			sj := j
			for j < len(rb) && unicode.IsDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			// Equal values: fewer leading zeros first.
			if (i - si) != (j - sj) {
				return (i - si) < (j - sj)
			}
			continue
		}
		la, lb := unicode.ToLower(ca), unicode.ToLower(cb)
		if la != lb {
			return la < lb
		}
		i++
		j++
	}
	if len(ra)-i != len(rb)-j {
		return len(ra)-i < len(rb)-j
	}
	return a < b
}
