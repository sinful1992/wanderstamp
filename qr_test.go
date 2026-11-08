package main

// Task 136: the TV pairing screen's QR code. The QR is decoded with an
// independent reader (gozxing), not the encoder that drew it, so a wrong
// drawing fails here and not only on someone's phone.

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	// PURE_BARCODE: the image is the code itself, so the reader skips finder
	// pattern detection, which misses one version-8 code (length 128, see
	// TestQRBreakEveryLengthDecodes) that jsQR and node-qrcode both accept:
	// rsc.io/qr's matrix for it is bit-identical to node-qrcode's mask 0.
	res, err := gzqr.NewQRCodeReader().Decode(bmp, map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_PURE_BARCODE: true})
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

// --- break tests from ada's review (#9736), written by a fresh subagent that saw
// only the contract and the diff ---

const qrMaxBytesM = 2331 // byte mode, version 40, level M

// A base of the length that makes pairURL exactly total bytes long.
func baseForTotal(total int) string {
	const suffix = len("/#pair=ABCD")
	pad := total - suffix - len(qrBase) - 1
	return qrBase + "/" + strings.Repeat("a", pad)
}

func TestQRBreakPublicURLNoHostname(t *testing.T) {
	for _, in := range []string{"https://:8448", "http://:80", "https://:8448/"} {
		if got, err := cleanPublicURL(in); err == nil {
			t.Errorf("%q accepted as %q: the QR has no host to open", in, got)
		}
	}
}

func TestQRBreakPublicURLOddButValid(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Example.com/":        "https://Example.com",
		"http://[::1]:8095/":          "http://[::1]:8095",
		"https://example.com/a%2Fb/":  "https://example.com/a%2Fb",
		"https://example.com/a/b///":  "https://example.com/a/b",
		"\thttps://example.com:8448 ": "https://example.com:8448",
	} {
		got, err := cleanPublicURL(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestQRBreakPublicURLRejected(t *testing.T) {
	for _, in := range []string{
		"", "   ", "https:example.com", "https:///path", "https://example.com/%zz",
		"https://exam\nple.com", `https://example.com\evil`, "https://example.com:port",
		"https://u@example.com", "https://example.com/a?", "HTTPS://example.com/#x",
	} {
		if got, err := cleanPublicURL(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestQRBreakPrefixKeptInPairURL(t *testing.T) {
	base, err := cleanPublicURL("https://example.com/w%20s/")
	if err != nil {
		t.Fatal(err)
	}
	f := newTVFix(t)
	f.a.publicURL = base
	p, _ := startPair(t, f)
	want := "https://example.com/w%20s/#pair=" + p.Code
	if p.PairURL != want {
		t.Fatalf("pair_url %q, want %q", p.PairURL, want)
	}
	if got := decodeQRSVG(t, p.QRSVG); got != want {
		t.Fatalf("QR reads %q, want %q", got, want)
	}
}

func TestQRBreakOverflowFailsOpen(t *testing.T) {
	for _, total := range []int{qrMaxBytesM + 1, 5000, 100000} {
		f := newTVFix(t)
		f.a.publicURL = baseForTotal(total)
		_, raw := startPair(t, f)
		if len(raw) != 3 || raw["code"] == nil || raw["poll"] == nil || raw["expires_in"] == nil {
			t.Errorf("total %d: want the plain 3-field response, got %d fields", total, len(raw))
		}
		if tok := f.pair(t, "Den"); tok == "" {
			t.Errorf("total %d: typed code did not pair", total)
		}
	}
}

func TestQRBreakCapacityEdge(t *testing.T) {
	if _, err := qrSVG(strings.Repeat("a", qrMaxBytesM+1)); err == nil {
		t.Error("one byte over version 40-M must be an error")
	}
	svg, err := qrSVG(strings.Repeat("a", qrMaxBytesM))
	if err != nil {
		t.Fatalf("largest text that fits: %v", err)
	}
	if !strings.Contains(svg, `viewBox="0 0 185 185"`) { // 177 + 2*4
		t.Errorf("not version 40: %.120s", svg)
	}
}

func TestQRBreakLargestPairURLDecodes(t *testing.T) {
	f := newTVFix(t)
	f.a.publicURL = baseForTotal(qrMaxBytesM)
	p, _ := startPair(t, f)
	if p.QRSVG == "" {
		t.Fatal("no QR for a URL that fits")
	}
	if got := decodeQRSVG(t, p.QRSVG); got != p.PairURL {
		t.Fatalf("QR reads %d bytes, want %d", len(got), len(p.PairURL))
	}
}

// Every length up to 400 crosses the small version boundaries.
func TestQRBreakEveryLengthDecodes(t *testing.T) {
	for n := 11; n <= 400; n++ {
		want := "https://x/" + strings.Repeat("b", n-10)
		svg, err := qrSVG(want)
		if err != nil {
			t.Fatalf("length %d: %v", n, err)
		}
		if got := decodeQRSVG(t, svg); got != want {
			t.Fatalf("length %d reads %q", n, got)
		}
	}
}

func TestQRBreakRateLimitHasNoQR(t *testing.T) {
	a := newTestApp(t)
	a.publicURL = qrBase
	const peer = "192.168.1.50:4000"
	for i := 0; i < tvMaxNewPeer; i++ {
		rec := doorReq(a, "POST", "/tv/pair", peer, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "qr_svg") {
			t.Fatalf("code %d: %d %.80s", i, rec.Code, rec.Body.String())
		}
	}
	rec := doorReq(a, "POST", "/tv/pair", peer, nil)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if b := rec.Body.String(); strings.Contains(b, "qr_svg") || strings.Contains(b, "pair_url") {
		t.Fatalf("429 carries a QR: %.120s", b)
	}
}

func TestQRBreakEvictedCodeDead(t *testing.T) {
	f := newTVFix(t)
	f.a.publicURL = qrBase
	var ps []pairStart
	for i := 0; i < tvMaxOpenPeer+1; i++ {
		p, _ := startPair(t, f)
		ps = append(ps, p)
	}
	last := ps[len(ps)-1]
	if got := decodeQRSVG(t, last.QRSVG); got != qrBase+"/#pair="+last.Code {
		t.Fatalf("newest QR reads %q", got)
	}
	body := func(code string) string { return `{"code":"` + code + `","name":"Den"}` }
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", body(ps[0].Code), f.signedIn()); r.code != 404 {
		t.Fatalf("code of a replaced QR: %d, want 404", r.code)
	}
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", body(last.Code), f.signedIn()); r.code != 201 {
		t.Fatalf("code of the newest QR: %d %s", r.code, r.body)
	}
}

func TestQRBreakConcurrentCodesOwnQR(t *testing.T) {
	a := newTestApp(t)
	a.publicURL = qrBase
	const n = 15
	bodies := make([][]byte, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := doorReq(a, "POST", "/tv/pair", fmt.Sprintf("192.168.1.%d:4000", 100+i), nil)
			codes[i], bodies[i] = rec.Code, rec.Body.Bytes()
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range bodies {
		if codes[i] != 200 {
			t.Fatalf("request %d: %d %s", i, codes[i], bodies[i])
		}
		var p pairStart
		if err := json.Unmarshal(bodies[i], &p); err != nil {
			t.Fatal(err)
		}
		if seen[p.Code] {
			t.Fatalf("code %s handed out twice", p.Code)
		}
		seen[p.Code] = true
		if got, want := decodeQRSVG(t, p.QRSVG), qrBase+"/#pair="+p.Code; got != want {
			t.Fatalf("QR reads %q, want %q", got, want)
		}
	}
}
