package service

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/safety"
)

const maxTranscript = 300 << 10

// ytClients are the innertube clients tried, in order, to list a video's
// caption tracks. The web client's caption URLs now require a proof-of-origin
// token; the mobile clients' URLs still return text.
var ytClients = []map[string]any{
	{"clientName": "IOS", "clientVersion": "20.10.4", "deviceModel": "iPhone16,2", "hl": "en"},
	{"clientName": "ANDROID_VR", "clientVersion": "1.62.27", "androidSdkVersion": 32, "hl": "en"},
	{"clientName": "ANDROID", "clientVersion": "20.10.38", "androidSdkVersion": 30, "hl": "en"},
}

var errNoTranscript = errors.New("no transcript")

// FetchTranscript returns a YouTube video's captions as plain text,
// preferring English and human-written tracks over auto-generated ones.
// This relies on YouTube's unofficial player endpoint and may stop working
// or be blocked from datacenter IPs; failures are recorded, not retried.
func (s *Service) FetchTranscript(ctx context.Context, videoID string) (lang, text string, err error) {
	for _, client := range ytClients {
		trackURL, trackLang, err := s.captionTrack(ctx, videoID, client)
		if err != nil || trackURL == "" {
			continue
		}
		text, err := s.captionText(ctx, trackURL)
		if err == nil && text != "" {
			return trackLang, text, nil
		}
	}
	return "", "", errNoTranscript
}

// captionTrack asks the player endpoint for a video's caption tracks and
// picks one.
func (s *Service) captionTrack(ctx context.Context, videoID string, client map[string]any) (string, string, error) {
	body, _ := json.Marshal(map[string]any{"context": map[string]any{"client": client}, "videoId": videoID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.youtube.com/youtubei/v1/player?prettyPrint=false", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.API.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("player status %d", resp.StatusCode)
	}
	var p struct {
		Captions struct {
			Renderer struct {
				Tracks []struct {
					BaseURL      string `json:"baseUrl"`
					LanguageCode string `json:"languageCode"`
					Kind         string `json:"kind"` // "asr" for auto-generated
				} `json:"captionTracks"`
			} `json:"playerCaptionsTracklistRenderer"`
		} `json:"captions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&p); err != nil {
		return "", "", err
	}
	tracks := p.Captions.Renderer.Tracks
	best, bestRank := -1, 99
	for i, t := range tracks {
		rank := 4
		switch {
		case strings.HasPrefix(t.LanguageCode, "en") && t.Kind != "asr":
			rank = 0
		case strings.HasPrefix(t.LanguageCode, "en"):
			rank = 1
		case t.Kind != "asr":
			rank = 2
		}
		if rank < bestRank {
			best, bestRank = i, rank
		}
	}
	if best < 0 || !strings.HasPrefix(tracks[best].BaseURL, "https://www.youtube.com/") {
		return "", "", nil
	}
	return tracks[best].BaseURL, tracks[best].LanguageCode, nil
}

// captionText downloads a caption track and flattens it to plain text. It
// accepts the json3 format and the srv3 XML format the mobile URLs return.
func (s *Service) captionText(ctx context.Context, trackURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trackURL+"&fmt=json3", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.API.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := safety.ReadCapped(resp.Body, 4<<20)
	if err != nil || resp.StatusCode != http.StatusOK || len(raw) == 0 {
		return "", fmt.Errorf("caption status %d", resp.StatusCode)
	}
	var parts []string
	var j struct {
		Events []struct {
			Segs []struct {
				UTF8 string `json:"utf8"`
			} `json:"segs"`
		} `json:"events"`
	}
	if json.Unmarshal(raw, &j) == nil && len(j.Events) > 0 {
		for _, e := range j.Events {
			for _, seg := range e.Segs {
				parts = append(parts, seg.UTF8)
			}
		}
	} else {
		parts = xmlText(raw)
	}
	text := safety.SanitizeText(html.UnescapeString(strings.Join(parts, " ")))
	if len(text) > maxTranscript {
		text = safety.Truncate(text, maxTranscript)
	}
	return text, nil
}

// xmlText collects the character data of a timedtext XML document.
func xmlText(raw []byte) []string {
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		if cd, ok := tok.(xml.CharData); ok {
			if t := strings.TrimSpace(string(cd)); t != "" {
				out = append(out, t)
			}
		}
	}
}

// BackfillTranscripts fetches transcripts for up to n videos that have none
// recorded yet, best-ranked first.
func (s *Service) BackfillTranscripts(ctx context.Context, n int) {
	rows, err := s.Q.VideosNeedingTranscript(ctx, int32(n))
	if err != nil {
		s.Log.Error("transcripts backfill", "err", err)
		return
	}
	got := 0
	defer func() {
		if len(rows) > 0 {
			s.Log.Info("transcripts batch", "tried", len(rows), "found", got)
		}
	}()
	for _, r := range rows {
		id, ok := safety.YouTubeID(r.UrlNorm)
		if !ok {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		lang, text, err := s.FetchTranscript(cctx, id)
		cancel()
		if err == nil {
			got++
		}
		if err := s.Q.SaveTranscript(ctx, db.SaveTranscriptParams{ItemID: r.ID, Lang: lang, Text: text, Ok: err == nil}); err != nil {
			s.Log.Warn("save transcript", "item", r.ID, "err", err)
		}
		time.Sleep(time.Second) // stay gentle with YouTube
	}
}
