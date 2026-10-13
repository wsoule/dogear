package web

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// ogCard is everything drawn on a share preview image. Only public data
// goes in, since the image is served to anyone with the URL.
type ogCard struct {
	Title  string   // "Wyat's dogear" or a tag title
	Byline string   // "by @wyat · 42 things"
	Items  []string // the first few item titles
	More   int      // items beyond the ones listed
	Footer string   // "dogear.fyi/u/wyat"
}

// key hashes the card's text, so a cached PNG is reused until anything
// drawn on it changes (a new save, a rename, a new display name).
func (c ogCard) key() string {
	h := sha256.New()
	for _, s := range append([]string{c.Title, c.Byline, c.Footer, strconv.Itoa(c.More)}, c.Items...) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

const ogW, ogH = 1200, 630

var (
	ogPaper  = color.RGBA{0xfb, 0xfa, 0xf7, 0xff}
	ogInk    = color.RGBA{0x1b, 0x1b, 0x1a, 0xff}
	ogMuted  = color.RGBA{0x7a, 0x78, 0x73, 0xff}
	ogRule   = color.RGBA{0xe6, 0xe3, 0xdc, 0xff}
	ogDesk   = color.RGBA{0xef, 0xec, 0xe5, 0xff}
	ogAccent = color.RGBA{0xc2, 0x41, 0x0c, 0xff}
)

// The preview uses Go Mono (embedded in x/image), since the site itself is
// monospace; the fonts are parsed once, on first use.
var (
	ogFontsOnce   sync.Once
	ogRegular     *opentype.Font
	ogBold        *opentype.Font
	errOGFontLoad error
)

func ogFonts() (regular, bold *opentype.Font, err error) {
	ogFontsOnce.Do(func() {
		if ogRegular, errOGFontLoad = opentype.Parse(gomono.TTF); errOGFontLoad == nil {
			ogBold, errOGFontLoad = opentype.Parse(gomonobold.TTF)
		}
	})
	return ogRegular, ogBold, errOGFontLoad
}

func ogFace(f *opentype.Font, size float64) font.Face {
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err) // only fails on a bad size, which is a programming error
	}
	return face
}

// renderOG draws a 1200×630 preview card as PNG: paper background, the
// folded corner mark top right, the title, byline, and a few item titles.
func renderOG(c ogCard) ([]byte, error) {
	regular, bold, err := ogFonts()
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, ogW, ogH))
	draw.Draw(img, img.Bounds(), image.NewUniform(ogPaper), image.Point{}, draw.Src)
	drawFold(img, 132)

	const left = 80
	textW := ogW - left - 180 // keep clear of the fold
	small := ogFace(bold, 28)
	drawText(img, small, ogAccent, left, 96, "dogear")

	// The title takes one line at full size, or two lines at a smaller size.
	y := 200
	big := ogFace(bold, 64)
	if title := fitText(big, c.Title, textW); title == c.Title {
		drawText(img, big, ogInk, left, y, title)
	} else {
		mid := ogFace(bold, 52)
		lines := wrapText(mid, c.Title, textW, 2)
		for i, l := range lines {
			if i > 0 {
				y += 62
			}
			drawText(img, mid, ogInk, left, y, l)
		}
	}
	y += 56
	drawText(img, ogFace(regular, 28), ogMuted, left, y, fitText(ogFace(regular, 28), c.Byline, ogW-2*left))

	y += 36
	fillRect(img, image.Rect(left, y, ogW-left, y+2), ogRule)

	row := ogFace(regular, 30)
	arrow := ogFace(bold, 30)
	y += 66
	more := c.More
	for i, t := range c.Items {
		// A two-line title leaves room for fewer rows above the footer;
		// rows that don't fit are counted in "+N more" instead.
		if y > ogH-100 {
			more += len(c.Items) - i
			break
		}
		drawText(img, arrow, ogAccent, left, y, "→")
		drawText(img, row, ogInk, left+44, y, fitText(row, t, ogW-2*left-44))
		y += 54
	}
	if len(c.Items) == 0 {
		drawText(img, row, ogMuted, left, y, "nothing here yet")
	}

	foot := ogFace(regular, 24)
	footer := c.Footer
	if more > 0 {
		footer = "+" + strconv.Itoa(more) + " more · " + footer
	}
	drawText(img, foot, ogMuted, left, ogH-52, fitText(foot, footer, ogW-2*left))

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawFold draws the brand's dog-ear in the top right corner: the cut-away
// triangle shows the desk, and the folded flap is outlined in ink.
func drawFold(img *image.RGBA, size float32) {
	w := float32(ogW)
	cut := vector.NewRasterizer(ogW, ogH)
	cut.MoveTo(w-size, 0)
	cut.LineTo(w, 0)
	cut.LineTo(w, size)
	cut.ClosePath()
	cut.Draw(img, img.Bounds(), image.NewUniform(ogDesk), image.Point{})

	// The flap's outline, as three thin quads (vector has no stroker).
	const t = 3
	line := func(x0, y0, x1, y1 float32) {
		r := vector.NewRasterizer(ogW, ogH)
		dx, dy := x1-x0, y1-y0
		n := float32(t/2) / float32(math.Hypot(float64(dx), float64(dy)))
		nx, ny := -dy*n, dx*n
		r.MoveTo(x0+nx, y0+ny)
		r.LineTo(x1+nx, y1+ny)
		r.LineTo(x1-nx, y1-ny)
		r.LineTo(x0-nx, y0-ny)
		r.ClosePath()
		r.Draw(img, img.Bounds(), image.NewUniform(ogInk), image.Point{})
	}
	line(w-size, 0, w-size, size)
	line(w-size-1.5, size, w, size)
	line(w-size, 0, w, size)
}

func fillRect(img *image.RGBA, r image.Rectangle, c color.Color) {
	draw.Draw(img, r, image.NewUniform(c), image.Point{}, draw.Src)
}

func drawText(img *image.RGBA, face font.Face, c color.Color, x, y int, s string) {
	d := font.Drawer{Dst: img, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

// cleanForFace drops characters the font has no glyph for (emoji, CJK) so
// they don't render as boxes, and collapses the whitespace left behind.
func cleanForFace(face font.Face, s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) {
			b.WriteRune(' ')
			continue
		}
		if _, ok := face.GlyphAdvance(r); ok {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// fitText returns s unchanged when it fits in width pixels, else as many
// leading characters as fit followed by an ellipsis.
func fitText(face font.Face, s string, width int) string {
	s = cleanForFace(face, s)
	limit := fixed.I(width)
	if font.MeasureString(face, s) <= limit {
		return s
	}
	rs := []rune(s)
	for n := len(rs) - 1; n > 0; n-- {
		head := string(rs[:n])
		if font.MeasureString(face, head+"…") > limit {
			continue
		}
		// Cut at the last word break when that keeps most of the line, so
		// titles end on a whole word.
		if i := strings.LastIndex(head, " "); i > len(head)*2/3 {
			head = head[:i]
		}
		return strings.TrimRight(head, " ") + "…"
	}
	return "…"
}

// wrapText breaks s into at most maxLines lines that fit width, ending the
// last line with an ellipsis when text is left over.
func wrapText(face font.Face, s string, width, maxLines int) []string {
	words := strings.Fields(cleanForFace(face, s))
	var lines []string
	cur := ""
	for i, w := range words {
		next := w
		if cur != "" {
			next = cur + " " + w
		}
		if font.MeasureString(face, next) <= fixed.I(width) || cur == "" {
			cur = next
			continue
		}
		if len(lines) == maxLines-1 {
			return append(lines, fitText(face, strings.Join(append([]string{cur}, words[i:]...), " "), width))
		}
		lines = append(lines, cur)
		cur = w
	}
	if cur != "" {
		lines = append(lines, fitText(face, cur, width))
	}
	return lines
}

// pngCache is a small LRU of rendered preview PNGs keyed by ogCard.key, so
// a link pasted into a busy chat renders once, not once per unfurl.
type pngCache struct {
	mu    sync.Mutex
	limit int
	order *list.List // front = most recently used
	byKey map[string]*list.Element
}

type pngEntry struct {
	key string
	png []byte
}

func newPNGCache(limit int) *pngCache {
	return &pngCache{limit: limit, order: list.New(), byKey: map[string]*list.Element{}}
}

func (c *pngCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byKey[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*pngEntry).png, true
	}
	return nil, false
}

func (c *pngCache) put(key string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byKey[key]; ok {
		c.order.MoveToFront(e)
		e.Value.(*pngEntry).png = b
		return
	}
	c.byKey[key] = c.order.PushFront(&pngEntry{key: key, png: b})
	for c.order.Len() > c.limit {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.byKey, last.Value.(*pngEntry).key)
	}
}
