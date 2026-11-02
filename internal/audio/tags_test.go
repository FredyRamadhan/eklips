package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func id3Frame23(id string, text string) []byte {
	data := append([]byte{3}, []byte(text)...) // UTF-8 encoding
	frame := make([]byte, 10+len(data))
	copy(frame[0:4], id)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(data)))
	copy(frame[10:], data)
	return frame
}

func id3Tag(frames ...[]byte) []byte {
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	hdr := make([]byte, 10)
	copy(hdr, "ID3")
	hdr[3] = 3 // v2.3
	size := len(body)
	hdr[6] = byte(size>>21) & 0x7f
	hdr[7] = byte(size>>14) & 0x7f
	hdr[8] = byte(size>>7) & 0x7f
	hdr[9] = byte(size) & 0x7f
	return append(hdr, body...)
}

func TestReadMetadataID3v23(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fallback-name.mp3")
	tag := id3Tag(
		id3Frame23("TIT2", "Song Title"),
		id3Frame23("TPE1", "The Artist"),
		id3Frame23("TALB", "The Album"),
		id3Frame23("TYER", "1999"),
		id3Frame23("TRCK", "03/12"),
	)
	if err := os.WriteFile(path, tag, 0o644); err != nil {
		t.Fatal(err)
	}
	md := ReadMetadata(path)
	if md.Title != "Song Title" || md.Artist != "The Artist" || md.Album != "The Album" || md.Year != "1999" || md.TrackNumber != 3 {
		t.Fatalf("unexpected metadata: %+v", md)
	}
	if got, want := md.Display(), "The Artist — Song Title (The Album)"; got != want {
		t.Fatalf("Display() = %q, want %q", got, want)
	}
}

func TestReadMetadataWAVInfo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.wav")
	info := []byte("INFO")
	add := func(id, val string) {
		b := []byte(val + "\x00")
		h := make([]byte, 8)
		copy(h, id)
		binary.LittleEndian.PutUint32(h[4:], uint32(len(b)))
		info = append(info, h...)
		info = append(info, b...)
		if len(b)%2 == 1 {
			info = append(info, 0)
		}
	}
	add("INAM", "Info Title")
	add("IART", "Info Artist")
	add("IPRD", "Info Album")
	add("ICRD", "2001")
	add("ITRK", "7")

	var buf []byte
	buf = append(buf, []byte("RIFF")...)
	buf = append(buf, make([]byte, 4)...)
	buf = append(buf, []byte("WAVE")...)
	buf = append(buf, []byte("fmt ")...)
	fmtChunk := make([]byte, 4+16)
	binary.LittleEndian.PutUint32(fmtChunk, 16)
	buf = append(buf, fmtChunk...)
	buf = append(buf, []byte("LIST")...)
	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(len(info)))
	buf = append(buf, size...)
	buf = append(buf, info...)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(buf)-8))
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	md := ReadMetadata(path)
	if md.Title != "Info Title" || md.Artist != "Info Artist" || md.Album != "Info Album" || md.Year != "2001" || md.TrackNumber != 7 {
		t.Fatalf("unexpected metadata: %+v", md)
	}
}

func vorbisComment(entries ...string) []byte {
	vendor := []byte("eklips-test")
	var b []byte
	tmp := make([]byte, 4)
	binary.LittleEndian.PutUint32(tmp, uint32(len(vendor)))
	b = append(b, tmp...)
	b = append(b, vendor...)
	binary.LittleEndian.PutUint32(tmp, uint32(len(entries)))
	b = append(b, tmp...)
	for _, e := range entries {
		binary.LittleEndian.PutUint32(tmp, uint32(len(e)))
		b = append(b, tmp...)
		b = append(b, []byte(e)...)
	}
	return b
}

func TestReadMetadataFLAC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.flac")
	comment := vorbisComment("TITLE=Flac Title", "ARTIST=Flac Artist", "ALBUM=Flac Album", "DATE=2010", "TRACKNUMBER=5")
	block := []byte{0x84, 0, 0, 0} // last block, type 4 (vorbis comment)
	block[1] = byte(len(comment) >> 16)
	block[2] = byte(len(comment) >> 8)
	block[3] = byte(len(comment))
	data := append([]byte("fLaC"), block...)
	data = append(data, comment...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	md := ReadMetadata(path)
	if md.Title != "Flac Title" || md.Artist != "Flac Artist" || md.Album != "Flac Album" || md.Year != "2010" || md.TrackNumber != 5 {
		t.Fatalf("unexpected metadata: %+v", md)
	}
}

func makeOggPage(seq uint32, packet []byte) []byte {
	hdr := make([]byte, 27)
	copy(hdr, "OggS")
	hdr[4] = 0
	hdr[5] = 0
	binary.LittleEndian.PutUint32(hdr[14:18], 1)
	binary.LittleEndian.PutUint32(hdr[18:22], seq)
	hdr[26] = 1
	return append(append(hdr, byte(len(packet))), packet...)
}

func TestReadMetadataOggVorbis(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.ogg")
	ident := append([]byte{1}, []byte("vorbis")...)
	comment := append([]byte{3}, []byte("vorbis")...)
	comment = append(comment, vorbisComment("TITLE=Ogg Title", "ARTIST=Ogg Artist", "ALBUM=Ogg Album")...)
	data := append(makeOggPage(0, ident), makeOggPage(1, comment)...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	md := ReadMetadata(path)
	if md.Title != "Ogg Title" || md.Artist != "Ogg Artist" || md.Album != "Ogg Album" {
		t.Fatalf("unexpected metadata: %+v", md)
	}
}

func TestMetadataFilenameFallback(t *testing.T) {
	md := metadataFromFilename("/music/07. Some Artist - A Song.flac")
	if md.TrackNumber != 7 || md.Artist != "Some Artist" || md.Title != "A Song" {
		t.Fatalf("unexpected fallback: %+v", md)
	}
	md = metadataFromFilename("/music/just-a-file.mp3")
	if md.Title != "just-a-file" || md.Artist != "" {
		t.Fatalf("unexpected fallback: %+v", md)
	}
	if !strings.Contains(md.Display(), "just-a-file") {
		t.Fatalf("Display missing title: %q", md.Display())
	}
}
