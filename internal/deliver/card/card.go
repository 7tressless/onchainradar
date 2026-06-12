// Package card renders a detected signal as a branded 1600x900 PNG for the Telegram
// channel post. The layout mirrors the dashboard's "On-Chain Blueprint" theme (warm
// paper, Archivo Black display, Space Mono data, vermilion accent): brand lockup on
// top, the signal subject as a huge headline, a one-line summary, a three-cell spec
// band, and an attestation footer. Rendering is pure Go (no browser, no network);
// fonts are embedded so a static binary stays self-contained.
package card

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"strconv"
	"strings"
	"sync"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
)

// Embedded brand fonts. Both families are licensed under the SIL Open Font License
// (fonts/OFL-*.txt) and were fetched from the Google Fonts repository:
// https://github.com/google/fonts/tree/main/ofl/archivoblack
// https://github.com/google/fonts/tree/main/ofl/spacemono
var (
	//go:embed fonts/ArchivoBlack.ttf
	archivoBlackTTF []byte
	//go:embed fonts/SpaceMono-Regular.ttf
	spaceMonoTTF []byte
	//go:embed fonts/SpaceMono-Bold.ttf
	spaceMonoBoldTTF []byte
)

// Tone selects a palette slot for a styled text run, so callers describe intent
// (primary, secondary, accent) and the palette stays internal to this package.
type Tone int

const (
	ToneText   Tone = iota // primary ink
	ToneSub                // secondary
	ToneFaint              // tertiary labels
	ToneAccent             // vermilion
	ToneType               // the signal family's color
)

// Run is one styled segment of the card's summary line.
type Run struct {
	Text string
	Tone Tone
	Bold bool
}

// Spec is one cell of the bottom data band: a small caps label over a large value.
// Hot draws the value in the signal family's color instead of primary ink.
type Spec struct {
	Label string
	Value string
	Hot   bool
}

// Data is everything the renderer needs for one card. All strings are plain text
// (no HTML); the caller derives them from the signal and its pool registry context.
type Data struct {
	Kind      string // signal family: flow, whale, smart, lst, liq, borrow, depeg
	TypeLabel string // caps label next to the color chip, e.g. "WHALE SWAP"
	SigID     string // top-right signal reference, e.g. "SIG #142"
	Headline  string // the huge subject line, e.g. "USDe / WMNT"
	Sub       []Run  // one summary line under the headline
	Specs     []Spec // up to three cells; extras are dropped
	// AttestTxShort is the abbreviated attestation tx for the footer. Empty renders
	// "ATTESTATION PENDING" (the recovery loop attests later; the image is immutable,
	// so it reflects the state at send time).
	AttestTxShort string
	Timestamp     string // footer timestamp, e.g. "12 JUN 2026 · 14:31 UTC"
}

// Canvas geometry. 2x the display size so Telegram's recompression keeps text crisp
// on both phone and desktop previews; type is sized to stay readable without zooming.
const (
	width  = 1600
	height = 900
	margin = 96.0
)

// Palette, lifted from the dashboard theme (web globals.css), light paper variant.
const (
	bgTopHex  = "#eae7e0"
	bgBotHex  = "#ddd9cd"
	textHex   = "#16150f"
	subHex    = "#4d4b42"
	faintHex  = "#807c6e"
	accentHex = "#e8412a"
	lineHex   = "#16150f"
)

// typeColors maps a signal family to its dashboard rail color.
var typeColors = map[string]string{
	"flow":   "#7d869c",
	"whale":  "#b08838",
	"smart":  "#4f8a6b",
	"lst":    "#4a6f93",
	"liq":    "#bf5a3a",
	"borrow": "#9a6a4e",
	"depeg":  "#e8412a",
}

var (
	archivo  *truetype.Font
	monoReg  *truetype.Font
	monoBold *truetype.Font

	// renderMu serializes Render: truetype faces buffer glyphs internally and are
	// not safe for concurrent use, and the face cache below is guarded by the same
	// lock. Signals are rare, so contention is irrelevant.
	renderMu  sync.Mutex
	faceCache = map[string]font.Face{}
)

func init() {
	archivo = mustParseFont(archivoBlackTTF, "ArchivoBlack")
	monoReg = mustParseFont(spaceMonoTTF, "SpaceMono-Regular")
	monoBold = mustParseFont(spaceMonoBoldTTF, "SpaceMono-Bold")
}

// mustParseFont parses an embedded font, panicking on failure: the assets are
// compile-time constants, so a parse error means a corrupted build, not input.
func mustParseFont(b []byte, name string) *truetype.Font {
	f, err := truetype.Parse(b)
	if err != nil {
		panic(fmt.Sprintf("card: embedded font %s: %v", name, err))
	}
	return f
}

// face returns a cached font.Face. HintingNone is load-bearing: with full hinting,
// many faces sharing one truetype.Font corrupt occasional glyphs (observed on Space
// Mono Bold's "M"); unhinted rendering is glyph-safe and indistinguishable at 2x.
func face(f *truetype.Font, pts float64) font.Face {
	key := fmt.Sprintf("%p:%g", f, pts)
	if fc, ok := faceCache[key]; ok {
		return fc
	}
	fc := truetype.NewFace(f, &truetype.Options{Size: pts, DPI: 72, Hinting: font.HintingNone})
	faceCache[key] = fc
	return fc
}

// Render draws the card and returns it PNG-encoded. It never mutates d.
func Render(d Data) ([]byte, error) {
	renderMu.Lock()
	defer renderMu.Unlock()

	dc := gg.NewContext(width, height)

	// Background: a near-flat vertical gradient so the paper does not read as one
	// dead fill, then the oversized brand mark cropped by the corner, then a soft
	// vignette for depth.
	grad := gg.NewLinearGradient(0, 0, 0, height)
	grad.AddColorStop(0, hexColor(bgTopHex, 1))
	grad.AddColorStop(1, hexColor(bgBotHex, 1))
	dc.SetFillStyle(grad)
	dc.DrawRectangle(0, 0, width, height)
	dc.Fill()

	watermark(dc)

	vig := gg.NewRadialGradient(width/2, height*0.42, height*0.45, width/2, height*0.42, width*0.80)
	vig.AddColorStop(0, hexColor("#000000", 0))
	vig.AddColorStop(1, hexColor("#000000", 0.10))
	dc.SetFillStyle(vig)
	dc.DrawRectangle(0, 0, width, height)
	dc.Fill()

	tc, ok := typeColors[d.Kind]
	if !ok {
		tc = subHex
	}

	topBar(dc)
	hairline(dc, margin, 132, width-margin, 0.12)

	// Type row: color chip + caps label left, the signal reference right.
	dc.SetHexColor(tc)
	dc.DrawRectangle(margin, 236-20, 20, 20)
	dc.Fill()
	setMono(dc, true, 38)
	dc.SetHexColor(tc)
	drawTracked(dc, d.TypeLabel, margin+48, 236, 9)
	if d.SigID != "" {
		setMono(dc, false, 28)
		dc.SetHexColor(faintHex)
		w := trackedWidth(dc, d.SigID, 3)
		drawTracked(dc, d.SigID, width-margin-w, 236, 3)
	}

	// Headline, shrunk stepwise until it fits the content width.
	size := 152.0
	dc.SetFontFace(face(archivo, size))
	for {
		w, _ := dc.MeasureString(d.Headline)
		if w <= width-2*margin+24 || size <= 72 {
			break
		}
		size -= 8
		dc.SetFontFace(face(archivo, size))
	}
	dc.SetHexColor(textHex)
	dc.DrawString(d.Headline, margin-6, 398)

	drawRuns(dc, d.Sub, tc, margin, 496, 44)

	specBand(dc, d.Specs, tc)
	footer(dc, d)

	// Thin full-bleed frame; Telegram rounds the bubble itself.
	setHexA(dc, lineHex, 0.12)
	dc.SetLineWidth(2)
	dc.DrawRectangle(1, 1, width-2, height-2)
	dc.Stroke()

	grain(dc, 4)

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, fmt.Errorf("card: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// topBar draws the site header lockup: the target mark, "OCR", a divider, and the
// long wordmark left; the network and a LIVE chip right.
func topBar(dc *gg.Context) {
	drawMark(dc, margin+19, 81, 19, 2.6)

	dc.SetFontFace(face(archivo, 34))
	dc.SetHexColor(textHex)
	dc.DrawString("OCR", margin+56, 94)
	ocrW, _ := dc.MeasureString("OCR")

	setHexA(dc, lineHex, 0.25)
	dc.SetLineWidth(2)
	dc.DrawLine(margin+56+ocrW+20, 64, margin+56+ocrW+20, 92)
	dc.Stroke()

	setMono(dc, false, 24)
	dc.SetHexColor(faintHex)
	drawTracked(dc, "ONCHAIN RADAR", margin+56+ocrW+40, 90, 4)

	setMono(dc, true, 32)
	dc.SetHexColor(accentHex)
	xLive := rightString(dc, "LIVE", width-margin, 94)
	dc.DrawCircle(xLive-22, 83, 8)
	dc.Fill()
	setMono(dc, false, 32)
	dc.SetHexColor(faintHex)
	rightString(dc, "MANTLE", xLive-56, 94)
}

// drawMark draws the OCR target mark (the site favicon): nested squares, vertical
// ticks, and a vermilion center dot, in the icon's 9:4:2.4 half-size ratios.
func drawMark(dc *gg.Context, cx, cy, half, lw float64) {
	in := half * 4 / 9
	setHexA(dc, textHex, 0.92)
	dc.SetLineWidth(lw)
	dc.DrawRectangle(cx-half, cy-half, 2*half, 2*half)
	dc.Stroke()
	dc.DrawRectangle(cx-in, cy-in, 2*in, 2*in)
	dc.Stroke()
	dc.DrawLine(cx, cy-half, cx, cy-in)
	dc.DrawLine(cx, cy+in, cx, cy+half)
	dc.Stroke()
	dc.SetHexColor(accentHex)
	dc.DrawCircle(cx, cy, half*2.4/9)
	dc.Fill()
}

// watermark draws the brand mark blown up and cropped by the right edge, its center
// dot kept as a faint blip in the whitespace beside the summary line.
func watermark(dc *gg.Context) {
	cx, cy, half := 1330.0, 530.0, 290.0
	in := half * 4 / 9
	setHexA(dc, lineHex, 0.055)
	dc.SetLineWidth(2)
	dc.DrawRectangle(cx-half, cy-half, 2*half, 2*half)
	dc.Stroke()
	dc.DrawRectangle(cx-in, cy-in, 2*in, 2*in)
	dc.Stroke()
	dc.DrawLine(cx, cy-half, cx, cy-in)
	dc.DrawLine(cx, cy+in, cx, cy+half)
	dc.Stroke()
	setHexA(dc, accentHex, 0.13)
	dc.DrawCircle(cx, cy, 24)
	dc.Fill()
	setHexA(dc, accentHex, 0.5)
	dc.DrawCircle(cx, cy, 8.5)
	dc.Fill()
}

// specBand draws up to three label-over-value cells separated by vertical hairlines.
func specBand(dc *gg.Context, specs []Spec, typeColor string) {
	hairline(dc, margin, 584, width-margin, 0.12)
	cols := [3]float64{margin, 576, 1056}
	for _, vx := range []float64{536, 1016} {
		setHexA(dc, lineHex, 0.09)
		dc.SetLineWidth(2)
		dc.DrawLine(vx, 602, vx, 740)
		dc.Stroke()
	}
	for i, s := range specs {
		if i == len(cols) {
			break
		}
		setMono(dc, true, 28)
		dc.SetHexColor(faintHex)
		drawTracked(dc, s.Label, cols[i], 650, 4)
		setMono(dc, true, 56)
		if s.Hot {
			dc.SetHexColor(typeColor)
		} else {
			dc.SetHexColor(textHex)
		}
		dc.DrawString(s.Value, cols[i], 726)
	}
}

// footer draws the attestation state left and the timestamp right.
func footer(dc *gg.Context, d Data) {
	hairline(dc, margin, 792, width-margin, 0.12)
	dc.SetHexColor(accentHex)
	dc.DrawCircle(margin+8, 845, 7)
	dc.Fill()

	label, tail := "RECORDED ON-CHAIN", d.AttestTxShort
	if tail == "" {
		label, tail = "ATTESTATION PENDING", ""
	}
	setMono(dc, true, 28)
	dc.SetHexColor(subHex)
	drawTracked(dc, label, margin+30, 854, 4)
	if tail != "" {
		lw := trackedWidth(dc, label, 4)
		setMono(dc, false, 28)
		dc.SetHexColor(faintHex)
		dc.DrawString("  ·  "+tail, margin+30+lw, 854)
	}
	if d.Timestamp != "" {
		setMono(dc, false, 28)
		dc.SetHexColor(faintHex)
		rightString(dc, d.Timestamp, width-margin, 854)
	}
}

// grain adds deterministic film grain (a cheap LCG over the pixel buffer), which
// breaks up flat fills and masks Telegram's JPEG recompression banding.
func grain(dc *gg.Context, amp int32) {
	img, ok := dc.Image().(*image.RGBA)
	if !ok {
		return
	}
	seed := uint32(0x4f6cdd1d)
	px := img.Pix
	for i := 0; i+3 < len(px); i += 4 {
		seed = seed*1664525 + 1013904223
		n := int32(int8(seed>>24)) * amp / 128
		for c := 0; c < 3; c++ {
			v := int32(px[i+c]) + n
			if v < 0 {
				v = 0
			} else if v > 255 {
				v = 255
			}
			px[i+c] = uint8(v)
		}
	}
}

func setMono(dc *gg.Context, bold bool, pts float64) {
	if bold {
		dc.SetFontFace(face(monoBold, pts))
	} else {
		dc.SetFontFace(face(monoReg, pts))
	}
}

// drawRuns draws the styled summary segments left to right at one font size.
func drawRuns(dc *gg.Context, runs []Run, typeColor string, x, y, pts float64) {
	for _, r := range runs {
		setMono(dc, r.Bold, pts)
		dc.SetHexColor(toneHex(r.Tone, typeColor))
		dc.DrawString(r.Text, x, y)
		w, _ := dc.MeasureString(r.Text)
		x += w
	}
}

func toneHex(t Tone, typeColor string) string {
	switch t {
	case ToneSub:
		return subHex
	case ToneFaint:
		return faintHex
	case ToneAccent:
		return accentHex
	case ToneType:
		return typeColor
	default:
		return textHex
	}
}

// drawTracked draws s with extra per-glyph spacing (letter-spacing for caps labels).
func drawTracked(dc *gg.Context, s string, x, y, extra float64) {
	for _, r := range s {
		cs := string(r)
		dc.DrawString(cs, x, y)
		w, _ := dc.MeasureString(cs)
		x += w + extra
	}
}

// trackedWidth measures what drawTracked would advance for s.
func trackedWidth(dc *gg.Context, s string, extra float64) float64 {
	var sum float64
	n := 0
	for _, r := range s {
		w, _ := dc.MeasureString(string(r))
		sum += w
		n++
	}
	if n > 1 {
		sum += extra * float64(n-1)
	}
	return sum
}

// rightString draws s right-aligned at xRight and returns the start x.
func rightString(dc *gg.Context, s string, xRight, y float64) float64 {
	w, _ := dc.MeasureString(s)
	dc.DrawString(s, xRight-w, y)
	return xRight - w
}

func hairline(dc *gg.Context, x1, y, x2, alpha float64) {
	setHexA(dc, lineHex, alpha)
	dc.SetLineWidth(2)
	dc.DrawLine(x1, y, x2, y)
	dc.Stroke()
}

func setHexA(dc *gg.Context, hex string, a float64) {
	r, g, b := hexRGB(hex)
	dc.SetRGBA(r, g, b, a)
}

func hexRGB(hex string) (float64, float64, float64) {
	v, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	return float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255
}

// rgba adapts a hex color to the color.Color gg's gradients expect.
type rgba struct{ r, g, b, a float64 }

func (c rgba) RGBA() (uint32, uint32, uint32, uint32) {
	conv := func(v float64) uint32 { return uint32(v * c.a * 0xffff) }
	return conv(c.r), conv(c.g), conv(c.b), uint32(c.a * 0xffff)
}

func hexColor(hex string, a float64) rgba {
	r, g, b := hexRGB(hex)
	return rgba{r, g, b, a}
}
