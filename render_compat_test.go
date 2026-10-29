package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	fitz "github.com/gen2brain/go-fitz"
	"github.com/go-pdf/fpdf"
)

func nativeTestPDF(t *testing.T) []byte {
	t.Helper()
	p := fpdf.New("P", "pt", "A4", "")
	for i := 0; i < 2; i++ {
		p.AddPage()
		p.SetFont("Helvetica", "", 20)
		p.Text(70, 100, fmt.Sprintf("SOURCE PAGE %d", i+1))
		if i == 0 {
			p.SetFillColor(255, 0, 0)
		} else {
			p.SetFillColor(0, 0, 255)
		}
		p.Rect(100, 200, 100, 100, "F")
	}
	var b bytes.Buffer
	if e := p.Output(&b); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

func compatibilityMarkdown() map[string]string {
	rows := "| Left | Right |\n|---|---|\n"
	for i := 0; i < 85; i++ {
		rows += fmt.Sprintf("| LEFT%03d %s | RIGHT%03d |\n", i, strings.Repeat("wrapping words ", 1+i%7), 999-i)
	}
	return map[string]string{
		"structure":         "# Alpha\n\nFirst **bold** and *italic* café.\n\n## Beta\n\n7. Seven\n8. Eight\n   - Nested asymmetric\n\n> Quoted left edge\n\n```go\nfirst < second\n  indented\n```\n\n~~not strikethrough~~\n",
		"escaped":           "<div>**literal tag bold content**</div>\n\nBefore <img src=\"file:///etc/passwd\"> after &amp; &#65;.\n\n![A & <B>](file:///etc/passwd)\n\n![](https://invalid.example/image.png)\n\nEND\n",
		"table_overflow":    "# Unequal cells\n\n" + rows + "\nAFTER TABLE\n",
		"cjk":               "# 多言語\n\n中文 日本語 한국어 café Ελληνικά русский العربية\n\nEND\n",
		"boundaries":        strings.Repeat("Unequal paragraph with **bold** words and a narrow ending.\n\n", 97) + "FINAL SENTINEL\n",
		"styled_images":     "![*bold* &amp; alt](https://example.com/image.png)\n\n![**nested _style_** `code` \\*literal\\* & <tag>][pic]\n\n![reference *alt*][]\n\n![](https://example.com/empty.png)\n\n[pic]: https://example.com/pic.png\n[reference *alt*]: https://example.com/reference.png\n",
		"clickable_links":   "[Visit](https://example.com/first?a=1&b=2) and [Other](https://example.com/second)\n\n<mailto:reader@example.com>\n\n[reference][ref]\n\n[" + strings.Repeat("wrapped asymmetric label ", 300) + "](https://example.com/across-pages)\n\n[ref]: https://example.com/reference\n",
		"link_destinations": "[Relative](chapter.xhtml?x=1) [Named](name:NextPage) [Unicode](https://example.com/caf%C3%A9)\n",
	}
}

func compatibilityEPUB(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	entries := [][2]string{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/book.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`},
		{"OEBPS/book.opf", `<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Test</dc:title><dc:identifier id="id">test</dc:identifier><dc:language>en</dc:language></metadata><manifest><item id="b" href="second.xhtml" media-type="application/xhtml+xml"/><item id="nav" href="nav.xhtml" properties="nav" media-type="application/xhtml+xml"/><item id="a" href="first%20chapter.xhtml" media-type="application/xhtml+xml"/><item id="extra" href="extra.xhtml" media-type="application/xhtml+xml"/><item id="cover" href="cover.xhtml" properties="cover" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="a"/><itemref idref="b"/></spine></package>`},
		{"OEBPS/second.xhtml", `<html><head><title>EXCLUDED HEAD</title><style>EXCLUDED CSS</style></head><body>EXCLUDED DIRECT BODY<p>SECOND <b>bold</b> tail &amp; entity.</p><script>EXCLUDED SCRIPT</script><p>中文</p></body></html>`},
		{"OEBPS/nav.xhtml", `<html><body><nav type="toc"><ol><li><a href="second.xhtml">Navigation</a></li></ol></nav></body></html>`},
		{"OEBPS/first chapter.xhtml", `<html><body><p>FIRST<br/>line</p><p>   whitespace   </p></body></html>`},
		{"OEBPS/extra.xhtml", `<html><body><p>NON SPINE</p><p>last</p></body></html>`},
		{"OEBPS/cover.xhtml", `<html><head><title>Discard this title</title></head><body><p>Discard this cover body</p></body></html>`},
	}
	for _, entry := range entries {
		w, err := z.Create(entry[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(entry[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestNativeEPUBManifestAndTextBoundaries(t *testing.T) {
	got, err := extractEPUB(context.Background(), compatibilityEPUB(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "SECOND\nbold\ntail & entity.\n中文\n\nNavigation\n\nFIRST\nline\nwhitespace\n\nNON SPINE\nlast\n\nCover"
	if got != want {
		t.Fatalf("EPUB text:\n got %q\nwant %q", got, want)
	}
	out, err := ExtractDocument(map[string][]byte{"bad.epub": []byte("invalid zip")}, false)
	if err != nil || out.SourceText != "" || len(out.Warnings) == 0 {
		t.Fatalf("invalid EPUB: %+v %v", out, err)
	}
	book := map[string][]byte{"d.epub": compatibilityEPUB(t)}
	out, err = ExtractDocument(book, false)
	if err != nil || out.PageCount != 0 {
		t.Fatalf("EPUB must not invent physical pages: %+v %v", out, err)
	}
	if n, err := DocumentPageCount(book); err != nil || n != 0 {
		t.Fatalf("EPUB page count must not use reflow pages: %d %v", n, err)
	}
}

func epubNormalizationCases() []struct{ html, want string } {
	return []struct{ html, want string }{
		{`<html><head><title>Head</title></head><body>DIRECT BODY<p>CHILD</p><table>FOSTER<tr><td>CELL</td></tr></table></body></html>`, "CHILD\nFOSTER\nCELL"},
		{`<html><body>text without elements</body></html>`, ""},
		{`<html><body>leading<p>FIRST <b>inline</b> end</p>retained tail<p>LAST</p>last tail</body></html>`, "FIRST\ninline\nend\nretained tail\nLAST\nlast tail"},
	}
}

func TestNativeEPUBBodyNormalization(t *testing.T) {
	for _, c := range epubNormalizationCases() {
		got, err := nativeEPUBHTMLText([]byte(c.html))
		if err != nil || got != c.want {
			t.Errorf("normalization %q: got %q want %q: %v", c.html, got, c.want, err)
		}
	}
}

func TestNativeMarkdownClickableLinksAndRawAlt(t *testing.T) {
	pdf, err := MarkdownPDF("![*bold* &amp; alt](https://example.com/image.png)\n\n[Visit](https://example.com)", "Title must retain links")
	if err != nil {
		t.Fatal(err)
	}
	nativeFitzMu.Lock()
	doc, err := fitz.NewFromMemory(pdf)
	if err != nil {
		nativeFitzMu.Unlock()
		t.Fatal(err)
	}
	text, textErr := doc.Text(0)
	links, linkErr := doc.Links(0)
	doc.Close()
	nativeFitzMu.Unlock()
	if textErr != nil || !strings.Contains(text, "[Image omitted: *bold* & alt]") {
		t.Fatalf("raw alt lost: %q %v", text, textErr)
	}
	if linkErr != nil || len(links) != 1 || links[0].URI != "https://example.com" {
		t.Fatalf("clickable links lost: %+v %v", links, linkErr)
	}
	for i := 0; i < 2; i++ {
		if _, err := MarkdownPDF("[missing](#no-such-id)", ""); err == nil || !strings.Contains(err.Error(), "No destination") {
			t.Fatalf("missing anchor must fail like reference: %v", err)
		}
	}
}

func TestNativeVectorPDFExportMetadata(t *testing.T) {
	p := fpdf.New("P", "pt", "A4", "")
	p.SetSubject("Original subject", true)
	p.SetKeywords("source-tag", true)
	for _, word := range []string{"ORIGINAL FIRST", "ORIGINAL SECOND"} {
		p.AddPage()
		p.SetFont("Helvetica", "", 16)
		p.Text(73, 111, word)
		p.SetFillColor(230, 20, 10)
		p.Rect(101, 203, 53, 79, "F")
	}
	var source bytes.Buffer
	if err := p.Output(&source); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"d.pdf": source.Bytes(), "d.content": []byte(`{"cPages":{"pages":[{"id":"b","redir":{"value":1}},{"id":"blank"},{"id":"a","redir":{"value":0}}]}}`)}
	metadata := map[string]string{"title": "Export title 中文", "subject": "reMarkable ID: document-id; path: /Folder/Title; source: pdf", "keywords": "document-id; new-tag", "creator": "remarkable-mcp", "producer": "native Go"}
	for _, mode := range []string{"merged", "annotations"} {
		t.Run(mode, func(t *testing.T) {
			pdf, info, err := ExportPDFMetadata(files, mode, metadata)
			if err != nil {
				t.Fatal(err)
			}
			if info.Pages != 3 || info.Status != "complete" {
				t.Fatalf("export info: %+v", info)
			}
			nativeFitzMu.Lock()
			defer nativeFitzMu.Unlock()
			doc, err := fitz.NewFromMemory(pdf)
			if err != nil {
				t.Fatal(err)
			}
			defer doc.Close()
			m := doc.Metadata()
			for key, value := range metadata {
				if !strings.Contains(m[key], value) {
					t.Errorf("missing metadata %s=%q: %q", key, value, m[key])
				}
			}
			if !strings.Contains(m["subject"], "Original subject") || !strings.Contains(m["keywords"], "source-tag") {
				t.Errorf("lost source attribution: subject=%q keywords=%q", m["subject"], m["keywords"])
			}
			for i, want := range []string{"ORIGINAL SECOND", "", "ORIGINAL FIRST"} {
				text, err := doc.Text(i)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "merged" && !strings.Contains(text, want) {
					t.Errorf("page %d lost selectable text: %q", i, text)
				}
				if (mode == "annotations" || i == 1) && strings.Contains(text, "ORIGINAL") {
					t.Errorf("page %d unexpected source text: %q", i, text)
				}
			}
		})
	}
}

func TestNativeExcerptBoundariesAndInvalidMapping(t *testing.T) {
	out, err := ExtractDocument(map[string][]byte{
		"d.content":    []byte(`{"text":"metadata excerpt"}`),
		"a.txt":        []byte("single\nmultiline excerpt"),
		"d.highlights": []byte(`{"highlights":[[{"text":"first\ncontinued"},{"text":"second"}]]}`),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.HighlightTexts) != 2 || out.HighlightTexts[0] != "first\ncontinued" || len(out.TypedTexts) != 2 || out.TypedTexts[0] != "single\nmultiline excerpt" || out.TypedTexts[1] != "metadata excerpt" {
		t.Fatalf("lost excerpt boundaries: %+v", out)
	}
	files := map[string][]byte{"d.pdf": nativeTestPDF(t), "d.content": []byte(`{"pages":["first","bad","last"],"redirectionPageMap":[0,99,1]}`)}
	pdf, info, err := ExportPDF(files, "merged")
	if err != nil || info.Status != "partial" || len(info.FailedPages) != 1 || info.FailedPages[0] != 2 {
		t.Fatalf("bad mapping export: %+v %v", info, err)
	}
	if n, err := PDFPageCount(pdf); err != nil || n != 3 {
		t.Fatalf("lost physical page: %d %v", n, err)
	}
}

func TestNativeStoryCompatibilityContent(t *testing.T) {
	for name, md := range compatibilityMarkdown() {
		t.Run(name, func(t *testing.T) {
			pdf, err := MarkdownPDF(md, "title")
			if err != nil {
				t.Fatal(err)
			}
			out, err := ExtractDocument(map[string][]byte{"d.pdf": pdf}, false)
			if err != nil {
				t.Fatal(err)
			}
			if out.PageCount == 0 || strings.Contains(out.SourceText, "root:x:") {
				t.Fatalf("invalid Story output: %+v", out)
			}
			if name == "cjk" {
				for _, word := range []string{"中文", "日本語", "한국어"} {
					if !strings.Contains(out.SourceText, word) {
						t.Fatalf("missing %s: %q", word, out.SourceText)
					}
				}
			}
			if name == "table_overflow" && (!strings.Contains(out.SourceText, "AFTER TABLE") || !strings.Contains(out.SourceText, "LEFT084") || out.PageCount < 3) {
				t.Fatalf("table lost content: %+v", out)
			}
		})
	}
}

// Opt-in external oracle only. Product code never starts Python. Set
// REMARKABLE_PYTHON_ORACLE to a Python with the reference remarkable_mcp and
// its dependencies installed (PYTHONPATH may also select the reference tree).
// Compare both PDFs using the same external extractor, so library extraction
// differences cannot masquerade as layout differences.
func TestPythonNativeRenderingDifferential(t *testing.T) {
	python := os.Getenv("REMARKABLE_PYTHON_ORACLE")
	if python == "" {
		t.Skip("set REMARKABLE_PYTHON_ORACLE to run the reference comparison")
	}
	fixtures := compatibilityMarkdown()
	type fixture struct {
		Markdown string
		Native   []byte
		HTML     string
	}
	input := struct {
		Fixtures       map[string]fixture
		EPUB           []byte
		Normalizations map[string]string
	}{Fixtures: map[string]fixture{}, EPUB: compatibilityEPUB(t)}
	input.Normalizations = map[string]string{}
	for _, c := range epubNormalizationCases() {
		text, err := nativeEPUBHTMLText([]byte(c.html))
		if err != nil {
			t.Fatal(err)
		}
		input.Normalizations[c.html] = text
	}
	for name, md := range fixtures {
		pdf, err := MarkdownPDF(md, "")
		if err != nil {
			t.Fatal(err)
		}
		input.Fixtures[name] = fixture{Markdown: md, Native: pdf}
	}
	anchorHTML := `<p id="start"><a href="#target">Forward asymmetric</a></p>` + strings.Repeat(`<p>paragraph for page boundary</p>`, 65) + `<p id="target">DESTINATION</p><p><a href="#start">Back to start</a> <a href="https://example.com/last">External</a></p><p id="target">Duplicate ignored</p>`
	anchorPDF, err := nativeStoryPDF(anchorHTML, markdownCSS)
	if err != nil {
		t.Fatal(err)
	}
	input.Fixtures["story_anchors"] = fixture{HTML: anchorHTML, Native: anchorPDF}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", `
import base64,json,sys,tempfile,pathlib
import pymupdf
from remarkable_mcp.markdown_pdf import render_markdown_pdf, _USER_CSS
from remarkable_mcp.extract import extract_text_from_epub
d=json.load(sys.stdin)
results={}
for name,f in d['Fixtures'].items():
    if f['HTML']:
        story=pymupdf.Story(html=f['HTML'],user_css=_USER_CSS)
        ref=story.write_with_links(lambda n,filled:(pymupdf.Rect(0,0,595,842),pymupdf.Rect(54,54,541,788),None))
    else:
        ref=pymupdf.open(stream=render_markdown_pdf(f['Markdown']),filetype='pdf')
    native=pymupdf.open(stream=base64.b64decode(f['Native']),filetype='pdf')
    errors=[]; max_delta=0; link_count=0
    if len(ref)!=len(native): errors.append('page count %d != %d'%(len(ref),len(native)))
    for p,(a,b) in enumerate(zip(ref,native)):
        al=a.get_links(); bl=b.get_links(); link_count+=len(al)
        if len(al)!=len(bl): errors.append('page %d link count %d != %d'%(p,len(al),len(bl)))
        for x,y in zip(al,bl):
            for key in ('kind','uri','page','nameddest','name','zoom'):
                if x.get(key)!=y.get(key): errors.append('page %d link %s differs: %r != %r'%(p,key,x.get(key),y.get(key)))
            for key in ('from','to'):
                if (key in x)!=(key in y): errors.append('page %d link missing %s'%(p,key))
                elif key in x and max(abs(u-v) for u,v in zip(x[key],y[key]))>0.05:
                    errors.append('page %d link %s differs: %r != %r'%(p,key,x[key],y[key]))
        aw=a.get_text('words'); bw=b.get_text('words')
        if [w[4] for w in aw] != [w[4] for w in bw]:
            errors.append('page %d word content/order differs: %r != %r'%(p,[w[4] for w in aw][:25],[w[4] for w in bw][:25]))
        else:
            for x,y in zip(aw,bw):
                max_delta=max(max_delta,max(abs(x[i]-y[i]) for i in range(4)))
    if max_delta>0.05: errors.append('word bounding boxes differ by %.6f pt'%max_delta)
    results[name]={'errors':errors,'pages':len(ref),'max_delta':max_delta,'links':link_count}
from ebooklib import epub
from bs4 import BeautifulSoup
errors=[]
for html,native in d['Normalizations'].items():
    book=epub.EpubBook(); item=epub.EpubHtml(); item.content=html.encode(); book.add_item(item)
    ref=BeautifulSoup(item.get_content(),'html.parser').get_text(separator='\n',strip=True)
    if ref!=native: errors.append('body normalization differs: %r != %r'%(ref,native))
results['epub_normalization']={'errors':errors,'pages':0,'links':0,'max_delta':0}
with tempfile.TemporaryDirectory() as td:
    p=pathlib.Path(td)/'fixture.epub'; p.write_bytes(base64.b64decode(d['EPUB']))
    text=extract_text_from_epub(p)
    if not text:
        from ebooklib import epub
        epub.read_epub(str(p),options={'ignore_ncx':True})
print(json.dumps({'results':results,'epub':text,'version':pymupdf.VersionBind}))
`)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("oracle: %v: %s", err, stderr.String())
	}
	var result struct {
		Results map[string]struct {
			Errors   []string
			Pages    int
			Links    int
			MaxDelta float64 `json:"max_delta"`
		}
		EPUB, Version string
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("oracle JSON: %v: %s", err, output)
	}
	for name, r := range result.Results {
		t.Run(name, func(t *testing.T) {
			t.Logf("Python %s: %d pages, %d matching links; max word coordinate delta %.6fpt", result.Version, r.Pages, r.Links, r.MaxDelta)
			if (name == "clickable_links" || name == "story_anchors") && r.Links == 0 {
				t.Error("link fixture must contain clickable annotations")
			}
			for _, e := range r.Errors {
				t.Error(e)
			}
		})
	}
	native, err := extractEPUB(context.Background(), input.EPUB)
	if err != nil {
		t.Fatal(err)
	}
	if native != result.EPUB {
		t.Errorf("EPUB native %q != Python %q", native, result.EPUB)
	}
}
