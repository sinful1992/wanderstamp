package main

// Task 136: the TV pairing screen's QR code. The QR is decoded with an
// independent reader (gozxing), not the encoder that drew it, so a wrong
// drawing fails here and not only on someone's phone.

import (
	"encoding/json"
	"encoding/xml"
	"image"
	"image/color"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/makiuchi-d/gozxing"
	gzqr "github.com/makiuchi-d/gozxing/qrcode"
)

const qrBase = "https://home-server.tail7bde5d.ts.net:8448"

type pairStart struct {
	Code      string `json:"code"`
	Poll      string `json:"poll"`
	ExpiresIn int    `json:"expires_in"`
	PairURL   string `json:"pair_url"`
	QRSVG     string `json:"qr_svg"`
}

func startPair(t *testing.T, f *tvFix) (pairStart, map[string]any) {
	t.Helper()
	r := tvDo(t, f.door.URL, "POST", "/tv/pair", "", nil)
	if r.code != 200 {
		t.Fatalf("POST /tv/pair: %d %s", r.code, r.body)
	}
	var out pairStart
	r.json(t, &out)
	var raw map[string]any
	r.json(t, &raw)
	return out, raw
}

var qrRun = regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`)

// decodeQRSVG paints the SVG's modules into an image (8 px a module) and
// reads it back with gozxing.
func decodeQRSVG(t *testing.T, svg string) string {
	t.Helper()
	var root struct {
		XMLName xml.Name
		ViewBox string `xml:"viewBox,attr"`
	}
	if err := xml.Unmarshal([]byte(svg), &root); err != nil || root.XMLName.Local != "svg" {
		t.Fatalf("not an svg (%v): %.80s", err, svg)
	}
	vb := strings.Fields(root.ViewBox)
	if len(vb) != 4 || vb[2] != vb[3] {
		t.Fatalf("viewBox %q", root.ViewBox)
	}
	n, _ := strconv.Atoi(vb[2])
	const px = 8
	img := image.NewGray(image.Rect(0, 0, n*px, n*px))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	runs := qrRun.FindAllStringSubmatch(svg, -1)
	if len(runs) == 0 {
		t.Fatalf("no modules in %.120s", svg)
	}
	for _, m := range runs {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		if m[3] != m[4] {
			t.Fatalf("run %q does not close", m[0])
		}
		if x < 4 || y < 4 || x+w > n-4 || y >= n-4 {
			t.Fatalf("module %q inside the quiet zone (size %d)", m[0], n)
		}
		for yy := y * px; yy < (y+1)*px; yy++ {
			for xx := x * px; xx < (x+w)*px; xx++ {
				img.SetGray(xx, yy, color.Gray{0})
			}
		}
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	res, err := gzqr.NewQRCodeReader().Decode(bmp, nil)
	if err != nil {
		t.Fatalf("QR does not decode: %v", err)
	}
	return res.GetText()
}

func TestQRPairOffWithoutPublicURL(t *testing.T) {
	f := newTVFix(t)
	_, raw := startPair(t, f)
	if len(raw) != 3 || raw["code"] == nil || raw["poll"] == nil || raw["expires_in"] == nil {
		t.Fatalf("PUBLIC_URL unset must leave the response as 1.15.4: %v", raw)
	}
}

func TestQRPairURLAndSVG(t *testing.T) {
	f := newTVFix(t)
	f.a.publicURL = qrBase
	seen := map[string]bool{}
	for i := 0; i < 3; i++ { // every new code gets its own QR
		p, _ := startPair(t, f)
		want := qrBase + "/#pair=" + p.Code
		if p.PairURL != want {
			t.Fatalf("pair_url %q, want %q", p.PairURL, want)
		}
		if got := decodeQRSVG(t, p.QRSVG); got != want {
			t.Fatalf("QR reads %q, want %q", got, want)
		}
		if strings.Contains(p.QRSVG, p.Poll) || strings.Contains(p.PairURL, p.Poll) {
			t.Fatal("the poll token must never be in the QR")
		}
		low := strings.ToLower(p.QRSVG)
		for _, bad := range []string{"<script", "onload", "href", "<text", "<foreignobject", "<image"} {
			if strings.Contains(low, bad) {
				t.Fatalf("svg holds %q", bad)
			}
		}
		if seen[p.QRSVG] {
			t.Fatal("two codes, same QR")
		}
		seen[p.QRSVG] = true
	}
}

// The QR changes nothing about pairing: the typed code still pairs.
func TestQRTypedCodeStillPairs(t *testing.T) {
	f := newTVFix(t)
	f.a.publicURL = qrBase
	if tok := f.pair(t, "Living room"); tok == "" {
		t.Fatal("no token")
	}
}

func TestQRLongPublicURL(t *testing.T) {
	f := newTVFix(t)
	f.a.publicURL = qrBase + "/" + strings.Repeat("trips/", 60)
	p, _ := startPair(t, f)
	if got := decodeQRSVG(t, p.QRSVG); got != p.PairURL {
		t.Fatalf("QR reads %q, want %q", got, p.PairURL)
	}
}

func TestQRPublicURLChecked(t *testing.T) {
	for in, want := range map[string]string{
		qrBase:                       qrBase,
		qrBase + "/":                 qrBase,
		qrBase + "//":                qrBase,
		"  " + qrBase + "/ \n":       qrBase,
		"http://192.168.1.15:8095":   "http://192.168.1.15:8095",
		"https://example.com/ws/":    "https://example.com/ws",
		"https://example.com/w%20s/": "https://example.com/w%20s",
	} {
		got, err := cleanPublicURL(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"javascript:alert(1)", "ftp://example.com", "example.com", "/relative", "https://",
		"https://example.com/?x=1", "https://example.com/#a", "https://example.com/#", "https://example.com/?",
		"https://user:pw@example.com", "https://exa mple.com", "data:text/html,hi",
	} {
		if got, err := cleanPublicURL(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

// The response stays valid JSON with the SVG in it (quotes are escaped).
func TestQRResponseIsJSON(t *testing.T) {
	f := newTVFix(t)
	f.a.publicURL = qrBase
	r := tvDo(t, f.door.URL, "POST", "/tv/pair", "", nil)
	if !json.Valid(r.body) || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Fatalf("%s %s", r.header.Get("Content-Type"), r.body)
	}
}
