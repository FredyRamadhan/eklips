// Package playlist parses, validates, and generates JSON playlists.
package playlist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/media"
)

// CurrentVersion is the only supported schema version.
const CurrentVersion = 1

// MaxFileSize bounds the size of playlist files we are willing to parse.
const MaxFileSize = 8 << 20

// Track is one playlist entry.
type Track struct {
	Path string `json:"path"`
}

// Playlist is the on-disk JSON document.
type Playlist struct {
	Version int     `json:"version"`
	Name    string  `json:"name"`
	Tracks  []Track `json:"tracks"`
}

// Loaded is a validated playlist with absolute track paths.
type Loaded struct {
	Name   string
	Source string   // absolute path of the playlist file
	Tracks []string // absolute track paths
}

// Load reads, parses, and validates the playlist at path. Relative track
// paths resolve against the playlist file's directory. Every track must exist
// and be a supported audio file.
func Load(path string, maxTracks int) (*Loaded, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.FileNotFound, err, "invalid path")
	}
	if !media.IsPlaylist(abs) {
		return nil, apperr.New(apperr.InvalidPlaylist, "%s is not a .json playlist", filepath.Base(abs))
	}
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apperr.New(apperr.FileNotFound, "playlist %s does not exist", abs)
		}
		return nil, apperr.Wrap(apperr.FileNotFound, err, "cannot open playlist")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, apperr.Wrap(apperr.InvalidPlaylist, err, "cannot read playlist")
	}
	if len(data) > MaxFileSize {
		return nil, apperr.New(apperr.InvalidPlaylist, "playlist exceeds %d bytes", MaxFileSize)
	}
	pl, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return Resolve(pl, abs, maxTracks)
}

// Parse decodes and validates the document structure (not the files).
func Parse(data []byte) (*Playlist, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var pl Playlist
	if err := dec.Decode(&pl); err != nil {
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		switch {
		case errors.As(err, &se):
			return nil, apperr.New(apperr.InvalidPlaylist, "malformed JSON at offset %d: %v", se.Offset, err)
		case errors.As(err, &te):
			return nil, apperr.New(apperr.InvalidPlaylist, "field %q has wrong type: %v", te.Field, err)
		default:
			return nil, apperr.Wrap(apperr.InvalidPlaylist, err, "malformed JSON")
		}
	}
	if dec.More() {
		return nil, apperr.New(apperr.InvalidPlaylist, "trailing data after JSON document")
	}
	if pl.Version == 0 {
		return nil, apperr.New(apperr.InvalidPlaylist, "missing required field \"version\"")
	}
	if pl.Version != CurrentVersion {
		return nil, apperr.New(apperr.InvalidPlaylist, "unsupported version %d (expected %d)", pl.Version, CurrentVersion)
	}
	if pl.Tracks == nil {
		return nil, apperr.New(apperr.InvalidPlaylist, "missing required field \"tracks\"")
	}
	if len(pl.Tracks) == 0 {
		return nil, apperr.New(apperr.InvalidPlaylist, "\"tracks\" must contain at least one entry")
	}
	for i, t := range pl.Tracks {
		if strings.TrimSpace(t.Path) == "" {
			return nil, apperr.New(apperr.InvalidPlaylist, "tracks[%d]: missing \"path\"", i)
		}
		if strings.ContainsRune(t.Path, 0) {
			return nil, apperr.New(apperr.InvalidPlaylist, "tracks[%d]: invalid path", i)
		}
	}
	return &pl, nil
}

// Resolve converts track paths to absolute paths relative to the playlist file
// and validates each track.
func Resolve(pl *Playlist, source string, maxTracks int) (*Loaded, error) {
	if maxTracks > 0 && len(pl.Tracks) > maxTracks {
		return nil, apperr.New(apperr.InvalidPlaylist, "playlist has %d tracks; maximum is %d", len(pl.Tracks), maxTracks)
	}
	base := filepath.Dir(source)
	out := &Loaded{Name: pl.Name, Source: source, Tracks: make([]string, 0, len(pl.Tracks))}
	for i, t := range pl.Tracks {
		p := t.Path
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[2:])
			}
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		p = filepath.Clean(p)
		if err := media.CheckAudioFile(p); err != nil {
			e := apperr.As(err)
			return nil, apperr.New(apperr.InvalidPlaylist, "tracks[%d] (%s): %s", i, t.Path, e.Message)
		}
		out.Tracks = append(out.Tracks, p)
	}
	if out.Name == "" {
		out.Name = strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	}
	return out, nil
}

// Template returns the starter playlist written by `playlist create`.
func Template() *Playlist {
	return &Playlist{
		Version: CurrentVersion,
		Name:    "My Playlist",
		Tracks:  []Track{{Path: "track01.mp3"}, {Path: "subdir/track02.flac"}},
	}
}

// FromDir builds a playlist from the audio files in dir, with paths relative
// to the directory containing outPath.
func FromDir(dir, outPath string, maxTracks int) (*Playlist, error) {
	files, err := media.Scan(dir, maxTracks)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, apperr.New(apperr.FileNotFound, "no supported audio files in %s", dir)
	}
	base := filepath.Dir(outPath)
	pl := &Playlist{Version: CurrentVersion, Name: filepath.Base(dir)}
	for _, f := range files {
		rel, err := filepath.Rel(base, f)
		if err != nil {
			rel = f
		}
		pl.Tracks = append(pl.Tracks, Track{Path: filepath.ToSlash(rel)})
	}
	return pl, nil
}

// Write writes pl to path atomically. It refuses to overwrite unless force.
func Write(pl *Playlist, path string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return apperr.New(apperr.FileExists, "%s already exists (use --force to overwrite)", path)
		}
	}
	data, err := json.MarshalIndent(pl, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.Internal, err, "encode playlist")
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return apperr.Wrap(apperr.FileNotFound, err, "create %s", dir)
	}
	tmp, err := os.CreateTemp(dir, ".playlist-*.json")
	if err != nil {
		return apperr.Wrap(apperr.Internal, err, "write playlist")
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return apperr.Wrap(apperr.Internal, err, "write playlist")
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return apperr.Wrap(apperr.Internal, err, "write playlist")
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return apperr.Wrap(apperr.Internal, err, "write playlist")
	}
	if !force {
		// Link fails if the destination appeared meanwhile.
		if err := os.Link(tmpName, path); err != nil {
			os.Remove(tmpName)
			if os.IsExist(err) {
				return apperr.New(apperr.FileExists, "%s already exists (use --force to overwrite)", path)
			}
			return apperr.Wrap(apperr.Internal, err, "write playlist")
		}
		os.Remove(tmpName)
		return nil
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return apperr.Wrap(apperr.Internal, err, "write playlist")
	}
	return nil
}

// String renders a short description.
func (l *Loaded) String() string {
	return fmt.Sprintf("%s (%d tracks)", l.Name, len(l.Tracks))
}
