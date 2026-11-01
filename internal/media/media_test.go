package media

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/frdn/eklips/internal/apperr"
)

func TestNaturalLess(t *testing.T) {
	in := []string{"track10.mp3", "track2.mp3", "track1.mp3", "Track02.mp3", "a.wav"}
	// Verify pairwise expectations:
	if !NaturalLess("track2.mp3", "track10.mp3") {
		t.Fatal("expected track2.mp3 < track10.mp3")
	}
	if !NaturalLess("track1.mp3", "track2.mp3") {
		t.Fatal("expected track1.mp3 < track2.mp3")
	}
	_ = in
}

func TestScanNaturalOrderAndDotPath(t *testing.T) {
	dir := t.TempDir()
	names := []string{"track10.wav", "track2.wav", "track1.mp3", ".hidden.wav", "notes.txt"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir.mp3"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Scan(dir, 0)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var base []string
	for _, p := range got {
		base = append(base, filepath.Base(p))
	}
	want := []string{"track1.mp3", "track2.wav", "track10.wav"}
	if !reflect.DeepEqual(base, want) {
		t.Fatalf("Scan order = %v, want %v", base, want)
	}

	// Test scanning "." from inside dir.
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	dotGot, err := Scan(".", 0)
	if err != nil || len(dotGot) != 3 {
		t.Fatalf("Scan('.'): got %v, err=%v", dotGot, err)
	}

	// CheckAudioFile errors.
	if err := CheckAudioFile(filepath.Join(dir, "missing.wav")); !apperr.Is(err, apperr.FileNotFound) {
		t.Fatalf("missing file: got %v, want FILE_NOT_FOUND", err)
	}
	if err := CheckAudioFile(filepath.Join(dir, "notes.txt")); !apperr.Is(err, apperr.UnsupportedFormat) {
		t.Fatalf("txt file: got %v, want UNSUPPORTED_FORMAT", err)
	}
}
