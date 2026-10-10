package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "image/gif" // register decoders for thumbnails
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"dogear/internal/db"
	"dogear/internal/safety"
)

const (
	thumbMaxW     = 480
	thumbMaxH     = 360
	imageMaxBytes = 6 << 20
)

// ErrNoImage means an item has no usable preview image.
var ErrNoImage = errors.New("no image")

// imageSource returns where an item's preview image comes from: GitHub's
// social card for repos, the YouTube thumbnail for videos, otherwise the
// page's discovered og:image. "" means none is known.
func imageSource(urlNorm string, imageURL *string) string {
	if owner, repo, ok := safety.GitHubRepo(urlNorm); ok {
		return "https://opengraph.githubassets.com/1/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	}
	if id, ok := safety.YouTubeID(urlNorm); ok {
		return "https://i.ytimg.com/vi/" + url.PathEscape(id) + "/hqdefault.jpg"
	}
	if imageURL != nil {
		return *imageURL
	}
	return ""
}

// hasImage reports whether a row should show a thumbnail.
func hasImage(r db.Item) bool {
	return r.ImageState != "none" && imageSource(r.UrlNorm, r.ImageUrl) != ""
}

// resolveImageURL turns an og:image value into an absolute http(s) URL
// relative to the page it came from, or "" when unusable.
func resolveImageURL(pageURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "data:") {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	u, err := base.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(u.String()) > 2048 {
		return ""
	}
	return u.String()
}

// thumbSem bounds concurrent image fetches so a page full of new thumbnails
// cannot exhaust the server.
var thumbSem = make(chan struct{}, 6)

// thumbFlight collapses concurrent requests for the same thumbnail.
var thumbFlight sync.Map // item id -> *sync.Mutex

// Thumb returns an item's cached thumbnail, fetching, resizing, and storing
// it on first use. Images are fetched by the server through the SSRF-safe
// client and re-encoded as JPEG, so visitors never contact third parties.
func (s *Service) Thumb(ctx context.Context, itemID int64) (string, []byte, error) {
	if t, err := s.Q.GetThumb(ctx, itemID); err == nil {
		return t.Mime, t.Data, nil
	}
	mu, _ := thumbFlight.LoadOrStore(itemID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if t, err := s.Q.GetThumb(ctx, itemID); err == nil {
		return t.Mime, t.Data, nil
	}
	it, err := s.Q.GetItem(ctx, itemID)
	if err != nil || it.Status != "live" || !hasImage(it) {
		return "", nil, ErrNoImage
	}
	select {
	case thumbSem <- struct{}{}:
		defer func() { <-thumbSem }()
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
	data, w, h, err := s.fetchThumb(ctx, imageSource(it.UrlNorm, it.ImageUrl))
	if err != nil {
		s.Log.Info("thumb unavailable", "item", itemID, "err", err)
		_ = s.Q.SetImageState(context.WithoutCancel(ctx), db.SetImageStateParams{ID: itemID, ImageState: "none"})
		return "", nil, ErrNoImage
	}
	if err := s.Q.SaveThumb(ctx, db.SaveThumbParams{ItemID: itemID, Mime: "image/jpeg", Data: data, Width: int32(w), Height: int32(h)}); err != nil {
		return "", nil, fmt.Errorf("save thumb: %w", err)
	}
	_ = s.Q.SetImageState(ctx, db.SetImageStateParams{ID: itemID, ImageState: "ok"})
	return "image/jpeg", data, nil
}

// fetchThumb downloads an image (size-capped), decodes it, scales it to fit
// thumbMaxW×thumbMaxH, and encodes it as JPEG.
func (s *Service) fetchThumb(ctx context.Context, src string) ([]byte, int, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Accept", "image/avif;q=0,image/webp,image/png,image/jpeg,image/gif;q=0.8")
	resp, err := s.Fetch.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
		return nil, 0, 0, fmt.Errorf("not an image: %s", ct)
	}
	body, err := safety.ReadCapped(resp.Body, imageMaxBytes+1)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(body) > imageMaxBytes {
		return nil, 0, 0, fmt.Errorf("image too large")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode config: %w", err)
	}
	// Refuse decompression bombs before allocating pixels.
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > 40_000_000 || cfg.Width < 16 || cfg.Height < 16 {
		return nil, 0, 0, fmt.Errorf("unusable dimensions %dx%d", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode: %w", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := min(1.0, float64(thumbMaxW)/float64(w), float64(thumbMaxH)/float64(h))
	tw, th := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	// Flatten transparency onto white so PNG logos don't turn black.
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 78}); err != nil {
		return nil, 0, 0, err
	}
	return out.Bytes(), tw, th, nil
}

// BackfillImages discovers og:image URLs for up to n live items that lack a
// deterministic image source, best-ranked first.
func (s *Service) BackfillImages(ctx context.Context, n int) {
	rows, err := s.Q.ItemsNeedingImage(ctx, int32(n))
	if err != nil {
		s.Log.Error("images backfill", "err", err)
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, r := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			img := ""
			if page, err := s.fetchPage(ctx, r.UrlNorm, 5*time.Second, 512<<10); err == nil {
				img = resolveImageURL(r.UrlNorm, page.Image)
			}
			if err := s.Q.SetImageURL(ctx, db.SetImageURLParams{ID: r.ID, ImageUrl: &img}); err != nil {
				s.Log.Warn("set image url", "item", r.ID, "err", err)
			}
		}()
	}
	wg.Wait()
}
