package importfile

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Column names each field may go by, across Pocket, Raindrop, Takeout, and
// hand-made spreadsheets. Matching is case-insensitive on trimmed headers.
var (
	urlCols     = []string{"url", "link", "href", "address", "given_url", "resolved_url"}
	titleCols   = []string{"title", "name", "given_title", "resolved_title"}
	noteCols    = []string{"note", "notes", "comment", "description"}
	tagCols     = []string{"tags", "tag", "labels"}
	folderCols  = []string{"folder", "collection"}
	dateCols    = []string{"time_added", "created", "created_at", "date_added", "added", "saved_at", "date", "timestamp"}
	videoCols   = []string{"video id", "video_id", "videoid"}
	videoDates  = []string{"playlist video creation timestamp", "time added", "added", "timestamp", "date"}
	raindropish = []string{"folder", "excerpt", "cover", "highlights"}
)

// csvTable is a parsed CSV: lowercased header names and the rows below it.
type csvTable struct {
	header []string
	rows   [][]string
}

// col returns the index of the first header that matches one of names, or -1.
func (t csvTable) col(names ...string) int {
	for _, n := range names {
		if i := slices.Index(t.header, n); i >= 0 {
			return i
		}
	}
	return -1
}

// readTable parses CSV leniently and finds the header row: the first row
// naming a URL or video id column. Takeout's older playlist files put a
// block of playlist details above the video list, so the header is not
// always the first line.
func readTable(data []byte) (csvTable, bool) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.ReuseRecord = false
	var t csvTable
	for {
		rec, err := r.Read()
		if err != nil {
			// A malformed line ends the file; what was read so far is kept.
			break
		}
		if t.header == nil {
			h := make([]string, len(rec))
			for i, f := range rec {
				h[i] = strings.ToLower(strings.TrimSpace(f))
			}
			cand := csvTable{header: h}
			if cand.col(urlCols...) >= 0 || cand.col(videoCols...) >= 0 {
				t.header = h
			}
			continue
		}
		t.rows = append(t.rows, rec)
	}
	return t, t.header != nil
}

// field returns a row's trimmed value at column i, or "".
func field(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// parseCSV reads a Pocket, Raindrop, YouTube Takeout, or generic CSV. The
// header decides which: Pocket has time_added, Raindrop has folder or
// excerpt columns, Takeout playlists have a video id column.
func parseCSV(name string, data []byte) (Format, []Entry, error) {
	t, ok := readTable(data)
	if !ok {
		return "", nil, fmt.Errorf("%w: expected a CSV with a url column, or a bookmarks HTML file", ErrUnknownFormat)
	}
	if t.col(urlCols...) < 0 {
		return YouTube, parseYouTube(name, t), nil
	}
	format := CSV
	switch {
	case t.col("time_added") >= 0:
		format = Pocket
	case t.col(raindropish...) >= 0:
		format = Raindrop
	}
	urlCol, titleCol, tagCol, folderCol, dateCol := t.col(urlCols...), t.col(titleCols...), t.col(tagCols...), t.col(folderCols...), t.col(dateCols...)
	noteCol := t.col(noteCols...)
	if format == Raindrop {
		// Raindrop's excerpt is the page's own description; only note is
		// the person's.
		noteCol = t.col("note")
	}
	var out []Entry
	for _, row := range t.rows {
		u := field(row, urlCol)
		if u == "" {
			continue
		}
		e := Entry{URL: u, Title: field(row, titleCol), Note: field(row, noteCol), SavedAt: parseTime(field(row, dateCol))}
		tags := field(row, tagCol)
		switch {
		case format == Pocket || strings.Contains(tags, "|"):
			e.Tags = splitTags(tags, "|")
		default:
			e.Tags = splitTags(tags, ",")
		}
		if f := field(row, folderCol); f != "" {
			e.Tags = append(folderTags(strings.Split(f, "/")), e.Tags...)
		}
		out = append(out, e)
		if len(out) > MaxEntries {
			break
		}
	}
	return format, out, nil
}

// parseYouTube turns a Takeout playlist CSV ("Liked videos.csv", or
// "<name>-videos.csv" for other playlists) into watch links tagged with the
// playlist's name.
func parseYouTube(name string, t csvTable) []Entry {
	idCol, dateCol := t.col(videoCols...), t.col(videoDates...)
	tag := playlistName(name)
	var out []Entry
	for _, row := range t.rows {
		id := field(row, idCol)
		if !validVideoID(id) {
			continue
		}
		e := Entry{URL: "https://www.youtube.com/watch?v=" + id, SavedAt: parseTime(field(row, dateCol))}
		if tag != "" {
			e.Tags = []string{tag}
		}
		out = append(out, e)
		if len(out) > MaxEntries {
			break
		}
	}
	return out
}

// playlistName derives a playlist's name from its Takeout file name:
// "Liked videos.csv" is "liked videos", "Road trip-videos.csv" is "road
// trip". A generic name gives "".
func playlistName(name string) string {
	base := strings.TrimSuffix(path.Base(strings.ReplaceAll(name, "\\", "/")), path.Ext(name))
	base = strings.TrimSuffix(base, "-videos")
	base = strings.TrimSpace(base)
	switch strings.ToLower(base) {
	case "", ".", "export", "playlist", "videos", "upload":
		return ""
	}
	return strings.ToLower(base)
}

// validVideoID reports whether s looks like a YouTube video id.
func validVideoID(s string) bool {
	if len(s) < 6 || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// containerFolders are the top-level folders browsers and bookmark apps
// create on their own; they say nothing about the links inside, so they do
// not become tags.
var containerFolders = map[string]bool{
	"bookmarks": true, "bookmarks bar": true, "bookmarks toolbar": true, "bookmarks menu": true,
	"other bookmarks": true, "mobile bookmarks": true, "favorites": true, "favourites": true,
	"favorites bar": true, "imported": true, "unsorted": true, "menu": true, "toolbar": true,
	"mozilla firefox": true, "speed dial": true,
}

// folderTags turns a folder path into tags, dropping container folders.
func folderTags(path []string) []string {
	var out []string
	for _, f := range path {
		f = strings.TrimSpace(f)
		if f == "" || containerFolders[strings.ToLower(f)] {
			continue
		}
		out = append(out, f)
	}
	return out
}
