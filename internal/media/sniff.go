// Package media turns the video, audio and animated pictures people post
// into files every browser plays (see PLAN.md, Media): a probe, a
// conversion through ffmpeg, a poster, and the facts a player box needs.
package media

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"strings"
)

// Sniff is the type the hub serves an upload with, read from its bytes.
// Go's sniffer knows an ISO media file only by an "mp4…" brand, so a
// QuickTime movie, an iPhone's .m4a or ffmpeg's own mp4 (major brand
// isom) came out as application/octet-stream and were served as
// downloads; the ftyp box names the family, and FLAC gets its own type.
func Sniff(head []byte) string {
	if t := ftypType(head); t != "" {
		return t
	}
	if bytes.HasPrefix(head, []byte("fLaC")) {
		return "audio/flac"
	}
	t := http.DetectContentType(head)
	if i := strings.Index(t, ";"); i > 0 {
		t = t[:i]
	}
	return t
}

// ftypType reads the brands of an ISO base media file's leading ftyp box:
// the major brand, then the compatible ones.
func ftypType(b []byte) string {
	if len(b) < 16 || string(b[4:8]) != "ftyp" {
		return ""
	}
	size := int(binary.BigEndian.Uint32(b[:4]))
	if size < 16 || size > len(b) {
		size = min(len(b), 256)
	}
	brands := []string{string(b[8:12])}
	for i := 16; i+4 <= size; i += 4 {
		brands = append(brands, string(b[i:i+4]))
	}
	for _, br := range brands {
		switch br {
		case "M4A ", "M4B ", "F4A ":
			return "audio/mp4"
		case "qt  ":
			return "video/quicktime"
		case "3gp4", "3gp5", "3gp6", "3gp7", "3g2a", "3g2b", "3g2c":
			return "video/3gpp"
		case "heic", "heix", "heim", "heis", "hevc", "mif1", "msf1", "avif", "avis":
			return "" // pictures: HEIF and AVIF, left to the default path
		}
	}
	for _, br := range brands {
		switch br {
		case "isom", "iso2", "iso3", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "dash", "M4V ", "M4VH", "M4VP", "f4v ", "mmp4", "MSNV", "NDAS":
			return "video/mp4"
		}
	}
	return ""
}
