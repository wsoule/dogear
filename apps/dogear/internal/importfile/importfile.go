// Package importfile reads the exports people bring from other services —
// Pocket and Raindrop CSV, browser and Pinboard bookmarks HTML, and YouTube
// playlists from Google Takeout — into one list of saved links with their
// titles, notes, tags, and save times. It only parses; deciding what Dogear
// does with each link is the service's job.
package importfile

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxBytes caps an uploaded file. A 10,000-link export is a few MB.
	MaxBytes = 20 << 20
	// MaxEntries caps how many links one import reads, matching the size of
	// an archive.
	MaxEntries = 10000
	// maxUnzipped bounds what a zip may expand to, so a zip bomb cannot
	// exhaust memory.
	maxUnzipped = 60 << 20
)

// Format names a supported export.
type Format string

const (
	Pocket   Format = "pocket"
	Raindrop Format = "raindrop"
	Netscape Format = "bookmarks"
	YouTube  Format = "youtube"
	// CSV is any other spreadsheet with a url column.
	CSV Format = "csv"
)

// Label is the human name of a format for the import summary.
func (f Format) Label() string {
	switch f {
	case Pocket:
		return "Pocket export"
	case Raindrop:
		return "Raindrop export"
	case Netscape:
		return "bookmarks file"
	case YouTube:
		return "YouTube playlist"
	case CSV:
		return "CSV file"
	}
	return string(f)
}

// Entry is one saved link from an export. SavedAt is zero when the export
// did not say when it was saved.
type Entry struct {
	URL     string
	Title   string
	Note    string
	Tags    []string
	SavedAt time.Time
}

var (
	// ErrTooLarge means the upload is over MaxBytes (or a zip expands past
	// its cap).
	ErrTooLarge = errors.New("file is too large")
	// ErrUnknownFormat means the file is not an export this package reads.
	ErrUnknownFormat = errors.New("unrecognized file")
)

// Result is a parsed upload: its format, its links, and whether links past
// MaxEntries were dropped.
type Result struct {
	Format    Format
	Entries   []Entry
	Truncated bool
}

// Parse reads an upload named name (the name helps tell YouTube playlists
// apart and names their tag) and detects its format from the content. A zip
// is opened and every export inside it is read, which covers Pocket's zipped
// export and a Google Takeout archive.
func Parse(name string, r io.Reader) (Result, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read upload: %w", err)
	}
	if len(data) > MaxBytes {
		return Result{}, ErrTooLarge
	}
	var res Result
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		res, err = parseZip(data)
	} else {
		res.Format, res.Entries, err = parseOne(name, data)
	}
	if err != nil {
		return Result{}, err
	}
	if len(res.Entries) > MaxEntries {
		res.Entries, res.Truncated = res.Entries[:MaxEntries], true
	}
	return res, nil
}

// parseZip reads every CSV and HTML file in a zip that looks like an export,
// skipping files it does not recognize (a Takeout archive holds much more
// than playlists). The format reported is the first one found.
func parseZip(data []byte) (Result, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Result{}, fmt.Errorf("%w: unreadable zip", ErrUnknownFormat)
	}
	var res Result
	var total int64
	for _, f := range zr.File {
		ext := strings.ToLower(path.Ext(f.Name))
		if f.FileInfo().IsDir() || strings.Contains(f.Name, "__MACOSX") || (ext != ".csv" && ext != ".html" && ext != ".htm") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(rc, MaxBytes+1))
		rc.Close()
		if err != nil {
			continue
		}
		total += int64(len(b))
		if len(b) > MaxBytes || total > maxUnzipped {
			return Result{}, ErrTooLarge
		}
		format, entries, err := parseOne(f.Name, b)
		if err != nil {
			continue
		}
		if res.Format == "" {
			res.Format = format
		}
		res.Entries = append(res.Entries, entries...)
		if len(res.Entries) > MaxEntries {
			break
		}
	}
	if res.Format == "" {
		return Result{}, fmt.Errorf("%w: no export found in the zip", ErrUnknownFormat)
	}
	return res, nil
}

// parseOne detects one file's format and parses it.
func parseOne(name string, data []byte) (Format, []Entry, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		data = []byte(strings.ToValidUTF8(string(data), "�"))
	}
	if looksLikeBookmarks(data) {
		return Netscape, parseNetscape(data), nil
	}
	return parseCSV(name, data)
}

// looksLikeBookmarks reports whether data is a Netscape bookmarks file, the
// HTML every browser, Pinboard, and Raindrop export. Other HTML (a Takeout
// watch history, say) has links but no <DT> list, so it is not mistaken for
// bookmarks.
func looksLikeBookmarks(data []byte) bool {
	head := strings.ToLower(string(data[:min(len(data), 4096)]))
	if strings.Contains(head, "netscape-bookmark-file") {
		return true
	}
	lower := bytes.ToLower(data[:min(len(data), 1<<20)])
	return bytes.Contains(lower, []byte("<dt>")) && bytes.Contains(lower, []byte("<a "))
}

// splitTags splits a tag field on sep, trimming blanks and dropping empties.
func splitTags(s, sep string) []string {
	var out []string
	for t := range strings.SplitSeq(s, sep) {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// parseTime reads the timestamps exports use: Unix seconds (Pocket,
// bookmarks ADD_DATE; Chrome and Firefox sometimes write microseconds) or
// ISO 8601 dates (Raindrop, Takeout). It returns zero when unreadable or
// implausible.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	var t time.Time
	if n, ok := parseInt(s); ok {
		switch {
		case n > 1e15: // microseconds
			t = time.UnixMicro(n)
		case n > 1e12: // milliseconds
			t = time.UnixMilli(n)
		default:
			t = time.Unix(n, 0)
		}
	} else {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05 MST",
			"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
			if p, err := time.Parse(layout, s); err == nil {
				t = p
				break
			}
		}
	}
	if t.Year() < 1995 || t.After(time.Now().Add(48*time.Hour)) {
		return time.Time{}
	}
	return t.UTC()
}

// parseInt parses a run of ASCII digits.
func parseInt(s string) (int64, bool) {
	if s == "" || len(s) > 18 {
		return 0, false
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}
