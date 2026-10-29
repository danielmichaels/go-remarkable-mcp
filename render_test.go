package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	rms "github.com/alexgorbatchev/go-rmscene"
	fitz "github.com/gen2brain/go-fitz"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestNativeNotebookAuthorAndExtract(t *testing.T) {
	page, e := NewNotebookPage("Hello native\nSecond paragraph café")
	if e != nil {
		t.Fatal(e)
	}
	written, e := AuthorPage(page, []map[string]any{{"points": [][]float64{{0, .1}, {1, .9}}, "tool": "fineliner", "color": "red"}})
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.HasPrefix(written, page) {
		t.Fatal("authoring rewrote original bytes")
	}
	s, e := rms.Parse(written)
	if e != nil {
		t.Fatal(e)
	}
	lines := s.Lines()
	if len(lines) != 1 || lines[0].Points[0].X != -702 || lines[0].Points[1].X != 702 {
		t.Fatalf("incorrect native coordinates: %+v", lines)
	}
	files := map[string][]byte{"doc.content": []byte(`{"pages":["p"],"fileType":"notebook"}`), "doc/p.rm": written}
	out, e := ExtractDocument(files, false)
	if e != nil {
		t.Fatal(e)
	}
	if out.TypedText != "Hello native\nSecond paragraph café" || out.PageCount != 1 || len(out.AnnotatedPages) != 1 {
		t.Fatalf("incorrect extracted result: %+v", out)
	}
	if len(out.TypedTexts) != 2 || out.TypedTexts[0] != "Hello native" || out.TypedTexts[1] != "Second paragraph café" {
		t.Fatalf("lost typed paragraph boundaries: %q", out.TypedTexts)
	}
	for _, format := range []string{"svg", "png"} {
		img, e := RenderPage(files, 1, RenderOptions{Format: format, Background: "#ffffff"})
		if e != nil {
			t.Fatal(e)
		}
		if len(img.Data) == 0 {
			t.Fatal("empty render")
		}
		if format == "svg" && !bytes.Contains(img.Data, []byte("Hello native")) {
			t.Fatal("typed text absent from SVG")
		}
	}
}
func TestNativePDFMappingCompositingAndExports(t *testing.T) {
	pdf := nativeTestPDF(t)
	files := map[string][]byte{"d.pdf": pdf, "d.content": []byte(`{"cPages":{"pages":[{"id":"second","redir":{"value":1}},{"id":"inserted"},{"id":"first","redir":{"value":0}}]},"redirectionPageMap":[0,1,0]}`)}
	count, e := DocumentPageCount(files)
	if e != nil || count != 3 {
		t.Fatalf("page count %d %v", count, e)
	}
	for _, page := range []int{1, 3} {
		img, e := RenderPage(files, page, RenderOptions{Format: "png"})
		if e != nil {
			t.Fatal(e)
		}
		decoded, e := png.Decode(bytes.NewReader(img.Data))
		if e != nil {
			t.Fatal(e)
		}
		r, _, b, _ := decoded.At(300, 500).RGBA()
		if page == 1 && b <= r {
			t.Fatal("reordered PDF page blue underlay missing")
		}
		if page == 3 && r <= b {
			t.Fatal("reordered PDF page red underlay missing")
		}
	}
	a, e := readRenderArchive(files)
	if e != nil {
		t.Fatal(e)
	}
	if a.pdfIndex(2) != -1 {
		t.Fatal("cPages missing redirect fell through to stale legacy mapping")
	}
	for _, mode := range []string{"merged", "annotations"} {
		data, info, e := ExportPDF(files, mode)
		if e != nil {
			t.Fatal(e)
		}
		n, e := PDFPageCount(data)
		if e != nil || n != 3 || info.Status != "complete" {
			t.Fatalf("export %s %d %+v %v", mode, n, info, e)
		}
	}
	files["d.content"] = []byte(`{"pages":["a","b","c"],"redirectionPageMap":[1,-1,0]}`)
	a, e = readRenderArchive(files)
	if e != nil || a.pdfIndex(1) != 1 || a.pdfIndex(2) != -1 || a.pdfIndex(3) != 0 {
		t.Fatalf("legacy mapping failed %v", e)
	}
}
func TestNativePDFExtractionAndMarkdown(t *testing.T) {
	p, e := MarkdownPDF("# Native title\n\nHello **world** café.\n\n- One\n- Two\n\n| A | B |\n|---|---|\n| X | Y |\n\n```go\nfmt.Println(42)\n```\n\n![private](file:///etc/passwd)\n\n"+strings.Repeat("Paragraph with enough text to force pagination.\n\n", 90), "Document title")
	if e != nil {
		t.Fatal(e)
	}
	n, e := PDFPageCount(p)
	if e != nil || n < 2 {
		t.Fatalf("expected paginated PDF, %d %v", n, e)
	}
	out, e := ExtractDocument(map[string][]byte{"d.pdf": p}, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, term := range []string{"Native title", "café", "![private]", "fmt.Println(42)"} {
		if !strings.Contains(out.SourceText, term) {
			t.Fatalf("missing PDF text %q", term)
		}
	}
	if strings.Contains(out.SourceText, "root:") {
		t.Fatal("loaded local image resource")
	}
	if _, e := MarkdownPDF(" ", ""); e == nil {
		t.Fatal("accepted empty markdown")
	}
}
func TestNativeExportFailurePreservesOrdinal(t *testing.T) {
	files := map[string][]byte{"d.content": []byte(`{"pages":["ok","broken","blank"]}`), "broken.rm": []byte("broken")}
	pdf, info, e := ExportPDF(files, "annotations")
	if e != nil {
		t.Fatal(e)
	}
	n, e := PDFPageCount(pdf)
	if e != nil || n != 3 || info.Status != "partial" || len(info.FailedPages) != 1 || info.FailedPages[0] != 2 {
		t.Fatalf("bad placeholder metadata %+v n=%d e=%v", info, n, e)
	}
}
func TestNativeLegacyVersions(t *testing.T) {
	for _, version := range []int{3, 4, 5} {
		var b bytes.Buffer
		header := fmt.Sprintf("reMarkable .lines file, version=%d          ", version)
		if len(header) != 43 {
			t.Fatal(len(header))
		}
		b.WriteString(header)
		for _, v := range []uint32{1, 1, 17, 0, 0} {
			binary.Write(&b, binary.LittleEndian, v)
		}
		binary.Write(&b, binary.LittleEndian, float32(2))
		if version == 5 {
			binary.Write(&b, binary.LittleEndian, float32(0))
		}
		binary.Write(&b, binary.LittleEndian, uint32(2))
		for _, p := range [][6]float32{{100, 100, 0, 0, 2, 1}, {500, 500, 0, 0, 2, 1}} {
			binary.Write(&b, binary.LittleEndian, p)
		}
		s, e := parseNativeScene(b.Bytes())
		if e != nil {
			t.Fatalf("v%d: %v", version, e)
		}
		svg, _ := s.elements()
		if !strings.Contains(svg, "path") {
			t.Fatalf("v%d ink missing", version)
		}
	}
}
func TestNativeEPUBExtraction(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	entries := map[string]string{"mimetype": "application/epub+zip", "META-INF/container.xml": `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="book.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`, "book.opf": `<package version="2.0" xmlns="http://www.idpf.org/2007/opf" unique-identifier="id"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Native EPUB</dc:title><dc:identifier id="id">test</dc:identifier><dc:language>en</dc:language></metadata><manifest><item id="p" href="page.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="p"/></spine></package>`, "page.xhtml": `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Test</title></head><body><p>Native EPUB source text.</p></body></html>`}
	for name, data := range entries {
		w, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		w.Write([]byte(data))
	}
	z.Close()
	out, e := ExtractDocument(map[string][]byte{"d.epub": b.Bytes()}, false)
	if e != nil || !strings.Contains(out.SourceText, "Native EPUB source text.") {
		t.Fatalf("EPUB failed: %+v %v", out, e)
	}
}
func TestNativeRealRMFixture(t *testing.T) {
	name := os.Getenv("REMARKABLE_NATIVE_RM_FIXTURE")
	if name == "" {
		t.Skip("set REMARKABLE_NATIVE_RM_FIXTURE to a read-only copied .rm fixture")
	}
	raw, e := os.ReadFile(name)
	if e != nil {
		t.Fatal(e)
	}
	s, e := parseNativeScene(raw)
	if e != nil {
		t.Fatal(e)
	}
	if len(s.blocks) == 0 {
		t.Fatal("fixture has no blocks")
	}
	files := map[string][]byte{"p.rm": raw}
	img, e := RenderPage(files, 1, RenderOptions{Format: "png"})
	if e != nil || len(img.Data) == 0 {
		t.Fatalf("real fixture render: %v", e)
	}
	written, e := AuthorPage(raw, []map[string]any{{"points": [][]float64{{.2, .2}, {.3, .3}}}})
	if e != nil || !bytes.HasPrefix(written, raw) {
		t.Fatalf("real fixture append: %v", e)
	}
	t.Logf("fixture blocks=%d size=%dx%d PNG=%d bytes", len(s.blocks), img.Width, img.Height, len(img.Data))
}
func TestNativeOCR(t *testing.T) {
	if _, e := exec.LookPath("tesseract"); e != nil {
		t.Skip("native tesseract executable is not installed")
	}
	t.Setenv("REMARKABLE_OCR_BACKEND", "tesseract")
	p, e := MarkdownPDF("# NATIVE OCR TEST\n\nThe quick brown fox jumps over the lazy dog.", "")
	if e != nil {
		t.Fatal(e)
	}
	nativeFitzMu.Lock()
	d, e := fitz.NewFromMemory(p)
	if e != nil {
		nativeFitzMu.Unlock()
		t.Fatal(e)
	}
	img, e := d.ImagePNG(0, 150)
	d.Close()
	nativeFitzMu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	text, backend, e := OCRImage(context.Background(), img)
	if e != nil || backend != "tesseract" || !strings.Contains(text, "NATIVE OCR TEST") {
		t.Fatalf("OCR %q %s %v", text, backend, e)
	}
}

type ocrTestTransport func(*http.Request) (*http.Response, error)

func (f ocrTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeGoogleOCRAuthentication(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"type":"authorized_user","client_id":"fixture","client_secret":"fixture","refresh_token":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentials)
	t.Setenv("REMARKABLE_OCR_BACKEND", "google")
	for _, key := range []string{"fixture-key", ""} {
		t.Run(map[bool]string{true: "api-key", false: "default-credentials"}[key != ""], func(t *testing.T) {
			t.Setenv("GOOGLE_VISION_API_KEY", key)
			client := &http.Client{Transport: ocrTestTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"responses":[{"fullTextAnnotation":{"text":" Native Google text "}}]}`
				if r.URL.Host == "oauth2.googleapis.com" {
					body = `{"access_token":"fixture-token","token_type":"Bearer","expires_in":3600}`
				} else if r.URL.Host != "vision.googleapis.com" || (key != "" && r.URL.Query().Get("key") != key) || (key == "" && r.Header.Get("Authorization") != "Bearer fixture-token") {
					return nil, errors.New("unexpected OCR destination or authorization")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			previous := http.DefaultClient
			http.DefaultClient = client
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx := context.WithValue(context.Background(), oauth2.HTTPClient, client)
			var pngData bytes.Buffer
			if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			text, backend, err := OCRImage(ctx, pngData.Bytes())
			if err != nil || text != "Native Google text" || backend != "google" {
				t.Fatalf("Google OCR: %q %s %v", text, backend, err)
			}
		})
	}
}

func TestNativeNotebookContentIdentityAndCanvas(t *testing.T) {
	author := "12345678-1234-4321-abcd-1234567890ab"
	page, e := NewNotebookPageWithAuthor("", author)
	if e != nil {
		t.Fatal(e)
	}
	content, e := NotebookContent("p", page)
	if e != nil {
		t.Fatal(e)
	}
	cp := content["cPages"].(map[string]any)
	gotAuthor := cp["uuids"].([]any)[0].(map[string]any)["first"]
	if gotAuthor != author || content["formatVersion"] != 2 {
		t.Fatalf("identity mismatch: %+v", content)
	}
	var extra bytes.Buffer
	w := rms.NewDataWriter(&extra)
	zero := rms.CrdtId{}
	w.WriteBlock(13, 0, 1, func(w *rms.DataWriter) error {
		w.WriteLwwId(1, zero, rms.CrdtId{Part2: 11})
		w.WriteLwwBool(2, zero, true)
		w.WriteLwwBool(3, zero, true)
		return w.WriteSubblock(5, func(w *rms.DataWriter) error { w.WriteUint32(1620); return w.WriteUint32(2160) })
	})
	page = append(page, extra.Bytes()...)
	files := map[string][]byte{"d.content": []byte(`{"pages":["blank","p"]}`), "p.rm": page}
	for _, ordinal := range []int{1, 2} {
		img, size, source, e := RenderCanvasPage(files, ordinal)
		if e != nil {
			t.Fatal(e)
		}
		if size != [2]float64{1620, 2160} || img.Width != 1404 || img.Height != 1872 || source != "strokes" {
			t.Fatalf("bad canvas %dx%d size=%v source=%s", img.Width, img.Height, size, source)
		}
	}
	files = map[string][]byte{"d.pdf": nativeTestPDF(t), "d.content": []byte(`{"cPages":{"pages":[{"id":"mapped","redir":{"value":1}},{"id":"added"}]}}`)}
	img, size, source, e := RenderCanvasPage(files, 1)
	if e != nil || source != "tablet_pdf" || size[0] < 1404 || img.Width < 1000 {
		t.Fatalf("mapped canvas %+v %v %s %v", img, size, source, e)
	}
	_, size, source, e = RenderCanvasPage(files, 2)
	if e != nil || source != "strokes" || size != [2]float64{1404, 1872} {
		t.Fatalf("inserted page canvas %v %s %v", size, source, e)
	}
}

func TestNativeHighlightsAndTransparentPNG(t *testing.T) {
	page, e := NewNotebookPage("")
	if e != nil {
		t.Fatal(e)
	}
	var extra bytes.Buffer
	w := rms.NewDataWriter(&extra)
	zero := rms.CrdtId{}
	for i, g := range []struct {
		text string
		y    float64
	}{{"lower highlight", 400}, {"upper highlight", 100}} {
		e = w.WriteBlock(3, 1, 1, func(w *rms.DataWriter) error {
			w.WriteId(1, rms.CrdtId{Part2: 11})
			w.WriteId(2, rms.CrdtId{Part1: 1, Part2: uint64(100 + i)})
			w.WriteId(3, zero)
			w.WriteId(4, zero)
			w.WriteTaggedInt(5, 0)
			return w.WriteSubblock(6, func(w *rms.DataWriter) error {
				w.WriteUint8(1)
				w.WriteTaggedInt(4, 3)
				w.WriteString(5, g.text)
				return w.WriteSubblock(6, func(w *rms.DataWriter) error {
					w.WriteVarUint(1)
					w.WriteFloat64(-100)
					w.WriteFloat64(g.y)
					w.WriteFloat64(400)
					return w.WriteFloat64(40)
				})
			})
		})
		if e != nil {
			t.Fatal(e)
		}
	}
	page = append(page, extra.Bytes()...)
	files := map[string][]byte{"p.rm": page}
	out, e := ExtractDocument(files, false)
	if e != nil || out.Highlights != "upper highlight\nlower highlight" || len(out.AnnotatedPages) != 1 {
		t.Fatalf("highlights %+v %v", out, e)
	}
	img, e := RenderPage(files, 1, RenderOptions{Format: "png", Background: "transparent"})
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := png.Decode(bytes.NewReader(img.Data))
	if e != nil {
		t.Fatal(e)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha != 0 {
		t.Fatalf("transparent page background alpha=%d", alpha)
	}
	maxAlpha := uint32(0)
	for y := 0; y < decoded.Bounds().Dy(); y++ {
		for x := 0; x < decoded.Bounds().Dx(); x++ {
			_, _, _, a := decoded.At(x, y).RGBA()
			if a > maxAlpha {
				maxAlpha = a
			}
		}
	}
	if maxAlpha < 18000 || maxAlpha > 28000 {
		t.Fatalf("highlight alpha not preserved: %d", maxAlpha)
	}
}

func TestNativeExtractionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := ExtractDocumentContext(ctx, map[string][]byte{}, true); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation lost: %v", e)
	}
}

func TestNativeEditedTextCRDT(t *testing.T) {
	var b bytes.Buffer
	w := rms.NewDataWriter(&b)
	zero := rms.CrdtId{}
	id := func(a uint8, n uint64) rms.CrdtId { return rms.CrdtId{Part1: a, Part2: n} }
	w.WriteId(1, zero)
	w.WriteSubblock(2, func(w *rms.DataWriter) error {
		w.WriteSubblock(1, func(w *rms.DataWriter) error {
			return w.WriteSubblock(1, func(w *rms.DataWriter) error {
				w.WriteVarUint(2)
				for _, it := range []struct {
					id, left, right rms.CrdtId
					text            string
				}{{id(1, 16), zero, zero, "abcd"}, {id(2, 30), id(1, 17), id(1, 18), "X"}} {
					w.WriteSubblock(0, func(w *rms.DataWriter) error {
						w.WriteId(2, it.id)
						w.WriteId(3, it.left)
						w.WriteId(4, it.right)
						w.WriteTaggedInt(5, 0)
						return w.WriteString(6, it.text)
					})
				}
				return nil
			})
		})
		return w.WriteSubblock(2, func(w *rms.DataWriter) error {
			return w.WriteSubblock(1, func(w *rms.DataWriter) error { return w.WriteVarUint(0) })
		})
	})
	w.WriteSubblock(3, func(w *rms.DataWriter) error { w.WriteFloat64(-468); return w.WriteFloat64(234) })
	w.WriteTaggedFloat(4, 936)
	text, e := readSceneText(b.Bytes())
	if e != nil {
		t.Fatal(e)
	}
	var got strings.Builder
	for _, c := range text.chars {
		got.WriteString(c.text)
	}
	if got.String() != "abXcd" {
		t.Fatalf("compressed-span edit reordered: %q", got.String())
	}
}
