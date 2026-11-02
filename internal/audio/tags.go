package audio

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Metadata is the display metadata extracted from an audio file.
type Metadata struct {
	Title       string `json:"title,omitempty"`
	Artist      string `json:"artist,omitempty"`
	Album       string `json:"album,omitempty"`
	Year        string `json:"year,omitempty"`
	TrackNumber int    `json:"track_number,omitempty"`
}

// Display renders metadata as "Artist — Title (Album)", falling back to the
// title (which is itself derived from the file name when tags are absent).
func (m Metadata) Display() string {
	title := strings.TrimSpace(m.Title)
	artist := strings.TrimSpace(m.Artist)
	album := strings.TrimSpace(m.Album)
	var out string
	switch {
	case artist != "" && title != "":
		out = artist + " — " + title
	case title != "":
		out = title
	case artist != "":
		out = artist
	default:
		return ""
	}
	if album != "" {
		out += " (" + album + ")"
	}
	return out
}

const tagReadLimit = 4 << 20 // never read more than 4 MiB of metadata

// ReadMetadata extracts tags from path. It never fails: when a tag is missing
// or the file cannot be parsed, it falls back to filename heuristics.
func ReadMetadata(path string) Metadata {
	md := Metadata{}
	f, err := os.Open(path)
	if err != nil {
		return metadataFromFilename(path)
	}
	defer f.Close()

	head := make([]byte, 12)
	n, _ := io.ReadFull(f, head)
	head = head[:n]

	var ok bool
	switch {
	case n >= 10 && string(head[:3]) == "ID3":
		ok = parseID3(f, head, &md)
	case n >= 4 && string(head[:4]) == "fLaC":
		ok = parseFLAC(f, head, &md)
	case n >= 4 && string(head[:4]) == "OggS":
		ok = parseOgg(f, head, &md)
	case n >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		ok = parseWAV(f, &md)
	}
	_ = ok
	if strings.TrimSpace(md.Title) == "" {
		fb := metadataFromFilename(path)
		md.Title = fb.Title
		if md.Artist == "" {
			md.Artist = fb.Artist
		}
		if md.TrackNumber == 0 {
			md.TrackNumber = fb.TrackNumber
		}
	}
	if strings.TrimSpace(md.Title) == "" {
		md.Title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return md
}

// FilenameMetadata derives display metadata from common "Artist - Title" and
// "01. Title" naming conventions without reading the file. It is used for very
// large queues where reading every tag up front would delay playback.
func FilenameMetadata(path string) Metadata { return metadataFromFilename(path) }

// metadataFromFilename derives metadata from common "Artist - Title" and
// "01. Title" naming conventions.
func metadataFromFilename(path string) Metadata {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	base = strings.TrimSpace(base)
	md := Metadata{}
	if base == "" {
		return md
	}

	// Strip a leading track number: "01. ", "01 - ", "1_", "01 ".
	if num, rest, ok := splitTrackNumber(base); ok {
		md.TrackNumber = num
		base = strings.TrimSpace(rest)
	}
	// Prefer " - " as artist/title separator, then " – " (en dash).
	for _, sep := range []string{" - ", " – ", " — "} {
		if i := strings.Index(base, sep); i > 0 {
			artist := strings.TrimSpace(base[:i])
			title := strings.TrimSpace(base[i+len(sep):])
			if artist != "" && title != "" {
				md.Artist = artist
				md.Title = title
				return md
			}
		}
	}
	md.Title = base
	return md
}

func splitTrackNumber(s string) (int, string, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i > 3 {
		return 0, s, false
	}
	num, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, s, false
	}
	rest := s[i:]
	if rest == "" {
		return 0, s, false
	}
	switch rest[0] {
	case '.', '-', '_', ' ', ')':
		rest = strings.TrimLeft(rest[1:], " .-_")
		if rest == "" {
			return 0, s, false
		}
		return num, rest, true
	default:
		return 0, s, false
	}
}

// --- ID3v2 ---------------------------------------------------------------

func parseID3(r io.ReadSeeker, head []byte, md *Metadata) bool {
	if len(head) < 10 {
		return false
	}
	version := int(head[3])
	flags := head[5]
	size := int(syncsafe(head[6:10]))
	if size <= 0 || size > tagReadLimit {
		return false
	}
	if _, err := r.Seek(10, io.SeekStart); err != nil {
		return false
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return false
	}
	if flags&0x80 != 0 { // whole-tag unsynchronisation
		body = deunsync(body)
	}
	pos := 0
	// Skip extended header.
	if flags&0x40 != 0 && len(body) >= 4 {
		if version >= 4 {
			pos = int(syncsafe(body[:4]))
		} else {
			pos = int(binary.BigEndian.Uint32(body[:4])) + 4
		}
		if pos < 0 || pos > len(body) {
			pos = 0
		}
	}
	found := false
	for pos < len(body) {
		var id string
		var frameSize int
		var frameFlags uint16
		switch version {
		case 2:
			if pos+6 > len(body) {
				return found
			}
			id = string(body[pos : pos+3])
			frameSize = int(body[pos+3])<<16 | int(body[pos+4])<<8 | int(body[pos+5])
			pos += 6
		default:
			if pos+10 > len(body) {
				return found
			}
			id = string(body[pos : pos+4])
			if version >= 4 {
				frameSize = int(syncsafe(body[pos+4 : pos+8]))
			} else {
				frameSize = int(binary.BigEndian.Uint32(body[pos+4 : pos+8]))
			}
			frameFlags = binary.BigEndian.Uint16(body[pos+8 : pos+10])
			pos += 10
		}
		if id == "" || strings.Trim(id, "\x00") == "" || id[0] == 0 {
			return found
		}
		if frameSize <= 0 || pos+frameSize > len(body) {
			return found
		}
		data := body[pos : pos+frameSize]
		pos += frameSize
		if version >= 4 {
			if frameFlags&0x000c != 0 { // compressed or encrypted
				continue
			}
			if frameFlags&0x0002 != 0 { // per-frame unsynchronisation
				data = deunsync(data)
			}
		} else if frameFlags&0x00c0 != 0 { // compressed or encrypted
			continue
		}
		var text string
		switch id {
		case "TIT2", "TT2":
			text = decodeID3Text(data)
			if text != "" {
				md.Title = text
				found = true
			}
		case "TPE1", "TP1":
			text = decodeID3Text(data)
			if text != "" {
				md.Artist = text
				found = true
			}
		case "TALB", "TAL":
			text = decodeID3Text(data)
			if text != "" {
				md.Album = text
				found = true
			}
		case "TYER", "TYE", "TDRC":
			text = decodeID3Text(data)
			if text != "" {
				md.Year = text
				found = true
			}
		case "TRCK", "TRK":
			text = decodeID3Text(data)
			if text != "" {
				if n, ok := parseTrackNumber(text); ok {
					md.TrackNumber = n
					found = true
				}
			}
		}
	}
	return found
}

func syncsafe(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

func deunsync(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xff && i+1 < len(b) && b[i+1] == 0x00 {
			i++
		}
	}
	return out
}

func decodeID3Text(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	enc := data[0]
	body := data[1:]
	var s string
	switch enc {
	case 0:
		// ISO-8859-1: map bytes directly to runes.
		rs := make([]rune, len(body))
		for i, c := range body {
			rs[i] = rune(c)
		}
		s = string(rs)
	case 1:
		s = decodeUTF16(body, true)
	case 2:
		s = decodeUTF16(body, false)
	case 3:
		s = string(body)
	default:
		// Some taggers write UTF-8 without an encoding byte.
		if utf8.Valid(data) {
			s = string(data)
		}
	}
	s = strings.Trim(s, "\x00")
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func decodeUTF16(b []byte, bomAllowed bool) string {
	if len(b) < 2 {
		return ""
	}
	little := false
	if bomAllowed {
		switch {
		case b[0] == 0xff && b[1] == 0xfe:
			little = true
			b = b[2:]
		case b[0] == 0xfe && b[1] == 0xff:
			little = false
			b = b[2:]
		default:
			little = true
		}
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		var v uint16
		if little {
			v = binary.LittleEndian.Uint16(b[i:])
		} else {
			v = binary.BigEndian.Uint16(b[i:])
		}
		u = append(u, v)
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

func parseTrackNumber(s string) (int, bool) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// --- FLAC / Vorbis comments ---------------------------------------------

func parseFLAC(r io.ReadSeeker, head []byte, md *Metadata) bool {
	// The caller already consumed 12 bytes; the first metadata block header
	// starts at byte 4.  Reseek to it.
	if _, err := r.Seek(4, io.SeekStart); err != nil {
		return false
	}
	return parseFLACFrom(r, md)
}

func parseFLACFrom(r io.Reader, md *Metadata) bool {
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return false
		}
		last := hdr[0]&0x80 != 0
		typ := hdr[0] & 0x7f
		length := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		if length < 0 || length > tagReadLimit {
			return false
		}
		if typ == 4 {
			block := make([]byte, length)
			if _, err := io.ReadFull(r, block); err != nil {
				return false
			}
			parseVorbisCommentBlock(block, md)
			return true
		}
		if _, err := io.CopyN(io.Discard, r, int64(length)); err != nil {
			return false
		}
		if last {
			return false
		}
	}
}

// parseVorbisCommentBlock parses the Vorbis comment payload (without a codec
// prefix): vendor length/string, comment count, then key=value comments.
func parseVorbisCommentBlock(b []byte, md *Metadata) bool {
	pos := 0
	readLE32 := func() (int, bool) {
		if pos+4 > len(b) {
			return 0, false
		}
		v := int(binary.LittleEndian.Uint32(b[pos : pos+4]))
		pos += 4
		return v, true
	}
	vlen, ok := readLE32()
	if !ok || vlen < 0 || pos+vlen > len(b) {
		return false
	}
	pos += vlen
	count, ok := readLE32()
	if !ok || count < 0 || count > 100000 {
		return false
	}
	found := false
	for i := 0; i < count && pos+4 <= len(b); i++ {
		n, ok := readLE32()
		if !ok || n < 0 || pos+n > len(b) {
			return found
		}
		comment := string(b[pos : pos+n])
		pos += n
		key, val, ok := strings.Cut(comment, "=")
		if !ok {
			continue
		}
		if applyComment(strings.ToUpper(strings.TrimSpace(key)), val, md) {
			found = true
		}
	}
	return found
}

func applyComment(key, val string, md *Metadata) bool {
	val = strings.TrimSpace(val)
	if val == "" {
		return false
	}
	switch key {
	case "TITLE":
		md.Title = val
	case "ARTIST", "ALBUMARTIST":
		if md.Artist == "" || key == "ARTIST" {
			md.Artist = val
		}
	case "ALBUM":
		md.Album = val
	case "DATE", "YEAR":
		md.Year = val
	case "TRACKNUMBER":
		if n, ok := parseTrackNumber(val); ok {
			md.TrackNumber = n
		} else {
			return false
		}
	default:
		return false
	}
	return true
}

// --- Ogg Vorbis/Opus -----------------------------------------------------

type oggPacketReader struct {
	r io.Reader
	// Remaining packets from a page that contained more than one packet.
	queue [][]byte
}

func (o *oggPacketReader) next() ([]byte, error) {
	if len(o.queue) > 0 {
		p := o.queue[0]
		o.queue = o.queue[1:]
		return p, nil
	}
	// Accumulate segments until a packet boundary (a segment shorter than 255
	// bytes) is seen.
	var packet []byte
	for {
		page, err := readOggPage(o.r)
		if err != nil {
			if len(packet) > 0 {
				return packet, nil
			}
			return nil, err
		}
		offset := 0
		for _, seg := range page.segments {
			if offset+int(seg) > len(page.data) {
				return nil, io.ErrUnexpectedEOF
			}
			packet = append(packet, page.data[offset:offset+int(seg)]...)
			offset += int(seg)
			if seg < 255 {
				if len(packet) > 0 {
					o.queue = append(o.queue, packet)
					packet = nil
				}
			}
		}
		if len(o.queue) > 0 {
			p := o.queue[0]
			o.queue = o.queue[1:]
			return p, nil
		}
		if page.eos {
			if len(packet) > 0 {
				return packet, nil
			}
			return nil, io.EOF
		}
	}
}

type oggPage struct {
	segments []byte
	data     []byte
	eos      bool
}

func readOggPage(r io.Reader) (oggPage, error) {
	var hdr [27]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return oggPage{}, err
	}
	if string(hdr[:4]) != "OggS" {
		return oggPage{}, io.ErrUnexpectedEOF
	}
	nseg := int(hdr[26])
	table := make([]byte, nseg)
	if _, err := io.ReadFull(r, table); err != nil {
		return oggPage{}, err
	}
	total := 0
	for _, s := range table {
		total += int(s)
	}
	if total > tagReadLimit {
		return oggPage{}, io.ErrUnexpectedEOF
	}
	data := make([]byte, total)
	if _, err := io.ReadFull(r, data); err != nil {
		return oggPage{}, err
	}
	return oggPage{segments: table, data: data, eos: hdr[5]&0x04 != 0}, nil
}

func parseOgg(r io.ReadSeeker, head []byte, md *Metadata) bool {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false
	}
	pr := &oggPacketReader{r: r}
	// Identification header.
	if _, err := pr.next(); err != nil {
		return false
	}
	comment, err := pr.next()
	if err != nil {
		return false
	}
	switch {
	case len(comment) >= 7 && string(comment[:7]) == "\x03vorbis":
		return parseVorbisCommentBlock(comment[7:], md)
	case len(comment) >= 8 && string(comment[:8]) == "OpusTags":
		return parseVorbisCommentBlock(comment[8:], md)
	}
	return false
}

// --- WAV RIFF INFO -------------------------------------------------------

func parseWAV(r io.ReadSeeker, md *Metadata) bool {
	if _, err := r.Seek(12, io.SeekStart); err != nil {
		return false
	}
	found := false
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return found
		}
		id := string(hdr[:4])
		size := int64(binary.LittleEndian.Uint32(hdr[4:]))
		if size < 0 || size > tagReadLimit {
			return found
		}
		switch id {
		case "LIST":
			data := make([]byte, size)
			if _, err := io.ReadFull(r, data); err != nil {
				return found
			}
			if len(data) >= 4 && string(data[:4]) == "INFO" {
				if parseRIFFInfo(data[4:], md) {
					found = true
				}
			}
		case "id3 ", "ID3 ":
			data := make([]byte, size)
			if _, err := io.ReadFull(r, data); err != nil {
				return found
			}
			if len(data) >= 3 && string(data[:3]) == "ID3" {
				sr := strings.NewReader(string(data))
				if parseID3(sr, data[:min(10, len(data))], md) {
					found = true
				}
			}
		default:
			if _, err := r.Seek(size, io.SeekCurrent); err != nil {
				return found
			}
		}
		if size%2 == 1 {
			if _, err := r.Seek(1, io.SeekCurrent); err != nil {
				return found
			}
		}
	}
}

func parseRIFFInfo(b []byte, md *Metadata) bool {
	found := false
	pos := 0
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		pos += 8
		if size < 0 || pos+size > len(b) {
			return found
		}
		val := strings.TrimRight(string(b[pos:pos+size]), "\x00")
		val = strings.TrimSpace(val)
		pos += size
		if size%2 == 1 {
			pos++
		}
		if val == "" {
			continue
		}
		switch id {
		case "INAM":
			md.Title = val
			found = true
		case "IART":
			md.Artist = val
			found = true
		case "IPRD":
			md.Album = val
			found = true
		case "ICRD":
			md.Year = val
			found = true
		case "ITRK":
			if n, ok := parseTrackNumber(val); ok {
				md.TrackNumber = n
				found = true
			}
		}
	}
	return found
}
