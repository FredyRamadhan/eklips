package playlist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frdn/eklips/internal/apperr"
)

func TestParseValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"malformed", `{`},
		{"missing version", `{"name":"x","tracks":[{"path":"a.mp3"}]}`},
		{"wrong version", `{"version":2,"name":"x","tracks":[{"path":"a.mp3"}]}`},
		{"missing tracks", `{"version":1,"name":"x"}`},
		{"empty tracks", `{"version":1,"name":"x","tracks":[]}`},
		{"empty path", `{"version":1,"name":"x","tracks":[{"path":"  "}]}`},
		{"unknown field", `{"version":1,"name":"x","extra":1,"tracks":[{"path":"a.mp3"}]}`},
	}
	for _, tc := range cases {
		if _, err := Parse([]byte(tc.json)); !apperr.Is(err, apperr.InvalidPlaylist) {
			t.Errorf("%s: got %v, want INVALID_PLAYLIST", tc.name, err)
		}
	}
}

func TestResolveRelativeToPlaylistAndCreate(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f1 := filepath.Join(root, "track01.wav")
	f2 := filepath.Join(sub, "track02.flac")
	_ = os.WriteFile(f1, []byte("wav"), 0o644)
	_ = os.WriteFile(f2, []byte("flac"), 0o644)

	plFile := filepath.Join(root, "list.json")
	if err := Write(&Playlist{
		Version: 1,
		Name:    "Test",
		Tracks:  []Track{{Path: "track01.wav"}, {Path: "sub/track02.flac"}},
	}, plFile, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Refuse overwrite without force.
	if err := Write(Template(), plFile, false); !apperr.Is(err, apperr.FileExists) {
		t.Fatalf("Write without force: got %v, want FILE_EXISTS", err)
	}

	// Load from a different working directory to verify paths resolve against
	// the playlist file's directory, not the process working directory.
	otherDir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	_ = os.Chdir(otherDir)

	loaded, err := Load(plFile, 100)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Tracks) != 2 || loaded.Tracks[0] != f1 || loaded.Tracks[1] != f2 {
		t.Fatalf("unexpected resolved tracks: %+v", loaded.Tracks)
	}

	// Offending track index is identified when a track is unsupported or missing.
	badPl := filepath.Join(root, "bad.json")
	_ = os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hi"), 0o644)
	_ = Write(&Playlist{
		Version: 1,
		Name:    "Bad",
		Tracks:  []Track{{Path: "track01.wav"}, {Path: "readme.txt"}},
	}, badPl, true)
	_, err = Load(badPl, 100)
	if !apperr.Is(err, apperr.InvalidPlaylist) || !strings.Contains(err.Error(), "tracks[1]") {
		t.Fatalf("expected INVALID_PLAYLIST mentioning tracks[1], got %v", err)
	}
}
