package main

// MuPDF is a native compiled dependency (AGPL-3.0-or-later/commercial), not
// a Python worker. See github.com/gen2brain/go-fitz/COPYING and MuPDF's license.
// Binary distributors must comply with those licenses or obtain a commercial
// MuPDF license. fpdf is MIT; goldmark is MIT.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	rms "github.com/alexgorbatchev/go-rmscene"
	fitz "github.com/gen2brain/go-fitz"
	"html"
	"image"
	"image/color"
	"image/png"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type RenderOptions struct {
	Background, Format string
	RenderMerged       *bool
}
type PageImage struct {
	Data          []byte
	MIMEType      string
	Width, Height int
}
type ExtractedDocument struct {
	SourceText, TypedText, Annotations, Highlights, OCRText string
	TypedTexts, HighlightTexts                              []string
	PageCount                                               int
	Warnings                                                []string
	AnnotatedPages                                          []map[string]any
	OCRPages                                                []string
	OCRBackend                                              string
	PageIDs                                                 []string
}
type ExportInfo struct {
	Pages       int
	FailedPages []int
	Warnings    []string
	Status      string
}

// MuPDF's shared context/store is not safe for concurrent use by this wrapper.
var nativeFitzMu sync.Mutex

func PDFPageCount(data []byte) (int, error) {
	nativeFitzMu.Lock()
	defer nativeFitzMu.Unlock()
	d, e := fitz.NewFromMemory(data)
	if e != nil {
		return 0, e
	}
	defer d.Close()
	return d.NumPage(), nil
}

type renderContent struct {
	Pages  []string `json:"pages"`
	CPages struct {
		Pages []struct {
			ID    string `json:"id"`
			Redir *struct {
				Value int `json:"value"`
			} `json:"redir"`
		} `json:"pages"`
	} `json:"cPages"`
	Redirection []int `json:"redirectionPageMap"`
	PageCount   int   `json:"pageCount"`
}
type renderArchive struct {
	files   map[string][]byte
	content renderContent
	order   []string
	pdf     []byte
	epub    []byte
	rm      map[string][]byte
}

func readRenderArchive(files map[string][]byte) (*renderArchive, error) {
	a := &renderArchive{files: files, rm: map[string][]byte{}}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	stem := ""
	for _, n := range names {
		if path.Ext(n) == ".content" {
			if e := json.Unmarshal(files[n], &a.content); e != nil {
				return nil, fmt.Errorf("invalid content: %w", e)
			}
			stem = strings.TrimSuffix(path.Base(n), ".content")
			break
		}
	}
	for _, n := range names {
		switch path.Ext(n) {
		case ".pdf":
			if a.pdf == nil || strings.TrimSuffix(path.Base(n), ".pdf") == stem {
				a.pdf = files[n]
			}
		case ".epub":
			if a.epub == nil {
				a.epub = files[n]
			}
		case ".rm":
			id := strings.TrimSuffix(path.Base(n), ".rm")
			if _, ok := a.rm[id]; ok {
				return nil, fmt.Errorf("duplicate page id %s", id)
			}
			a.rm[id] = files[n]
		}
	}
	if len(a.content.CPages.Pages) > 0 {
		for _, p := range a.content.CPages.Pages {
			a.order = append(a.order, p.ID)
		}
	} else {
		a.order = append(a.order, a.content.Pages...)
	}
	if len(a.order) == 0 {
		for id := range a.rm {
			a.order = append(a.order, id)
		}
		sort.Strings(a.order)
	}
	if len(a.order) == 0 {
		count := a.content.PageCount
		if len(a.pdf) > 0 {
			n, e := PDFPageCount(a.pdf)
			if e != nil {
				return nil, e
			}
			count = n
		}
		for i := 0; i < count; i++ {
			a.order = append(a.order, "")
		}
	}
	return a, nil
}
func (a *renderArchive) pdfIndex(page int) int {
	if len(a.content.CPages.Pages) > 0 {
		p := a.content.CPages.Pages[page-1]
		if p.Redir == nil {
			return -1
		}
		return p.Redir.Value
	}
	if len(a.content.Redirection) > 0 {
		if page > len(a.content.Redirection) {
			return -1
		}
		return a.content.Redirection[page-1]
	}
	return page - 1
}
func RenderPage(files map[string][]byte, page int, options RenderOptions) (PageImage, error) {
	a, e := readRenderArchive(files)
	if e != nil {
		return PageImage{}, e
	}
	return a.render(page, options, false)
}

// RenderCanvasPage prefers the native full scene coordinate system, exactly
// the grid AuthorPage uses. PDF-only pages use their mapped PDF underlay.
func RenderCanvasPage(files map[string][]byte, page int) (PageImage, [2]float64, string, error) {
	a, e := readRenderArchive(files)
	if e != nil {
		return PageImage{}, [2]float64{}, "", e
	}
	if page < 1 || page > len(a.order) {
		return PageImage{}, [2]float64{}, "", fmt.Errorf("page out of range")
	}
	w, h := a.paperSize()
	if raw, ok := a.rm[a.order[page-1]]; ok {
		s, err := parseNativeScene(raw)
		if err != nil {
			return PageImage{}, [2]float64{}, "", err
		}
		w, h = s.w, s.h
		merged := false
		img, err := a.render(page, RenderOptions{Background: "white", RenderMerged: &merged}, true)
		return img, [2]float64{w, h}, "strokes", err
	}
	if len(a.pdf) > 0 && a.pdfIndex(page) >= 0 {
		img, err := a.render(page, RenderOptions{Background: "white"}, true)
		ppu := .3177
		if w == 1620 && h == 2160 {
			ppu = .3144
		}
		return img, [2]float64{float64(img.Width) / 2 / ppu, float64(img.Height) / 2 / ppu}, "tablet_pdf", err
	}
	merged := false
	img, err := a.render(page, RenderOptions{Background: "white", RenderMerged: &merged}, true)
	return img, [2]float64{w, h}, "strokes", err
}
func (a *renderArchive) paperSize() (float64, float64) {
	for _, id := range a.order {
		if raw, ok := a.rm[id]; ok {
			if s, e := parseNativeScene(raw); e == nil {
				return s.w, s.h
			}
		}
	}
	return 1404, 1872
}

func (a *renderArchive) render(page int, o RenderOptions, full bool) (PageImage, error) {
	if page < 1 || page > len(a.order) {
		return PageImage{}, fmt.Errorf("page %d out of range (1-%d)", page, len(a.order))
	}
	format := strings.ToLower(o.Format)
	if format == "" {
		format = "png"
	}
	if format != "png" && format != "svg" {
		return PageImage{}, fmt.Errorf("unsupported image format %q", format)
	}
	paperW, paperH := a.paperSize()
	scene := &nativeScene{w: paperW, h: paperH}
	if raw, ok := a.rm[a.order[page-1]]; ok {
		var e error
		scene, e = parseNativeScene(raw)
		if e != nil {
			return PageImage{}, fmt.Errorf("page %d: %w", page, e)
		}
	}
	elements, bounds := scene.elements()
	x, y, w, h := -scene.w/2, 0.0, scene.w, scene.h
	outW, outH := int(w), int(h)
	if full {
		scale := 1872 / math.Max(w, h)
		outW, outH = int(math.Round(w*scale)), int(math.Round(h*scale))
	}
	merged := len(a.pdf) > 0 && (o.RenderMerged == nil || *o.RenderMerged)
	underlay := ""
	if merged {
		index := a.pdfIndex(page)
		if index >= 0 {
			nativeFitzMu.Lock()
			d, e := fitz.NewFromMemory(a.pdf)
			if e == nil {
				if index >= d.NumPage() {
					e = fmt.Errorf("mapped PDF page %d is out of range", index)
				} else {
					var img []byte
					img, e = d.ImagePNG(index, 144)
					if e == nil {
						var cfgErr error
						cfg, cfgErr := png.DecodeConfig(bytes.NewReader(img))
						e = cfgErr
						if e == nil {
							outW, outH = cfg.Width, cfg.Height
							ppu := .3177
							if scene.w == 1620 && scene.h == 2160 {
								ppu = .3144
							}
							w, h = float64(outW)/2/ppu, float64(outH)/2/ppu
							x = -w / 2
							if scene.legacy {
								x = -scene.w / 2
							}
							underlay = fmt.Sprintf(`<image x="%.4f" y="0" width="%.4f" height="%.4f" href="data:image/png;base64,%s" xlink:href="data:image/png;base64,%s"/>`, x, w, h, base64.StdEncoding.EncodeToString(img), base64.StdEncoding.EncodeToString(img))
						}
					}
				}
				d.Close()
			}
			nativeFitzMu.Unlock()
			if e != nil {
				return PageImage{}, e
			}
		}
	}
	if !merged && !full && !math.IsInf(bounds[0], 1) {
		x, y = bounds[0]-20, bounds[1]-20
		w, h = math.Max(1, bounds[2]-bounds[0]+40), math.Max(1, bounds[3]-bounds[1]+40)
		outW, outH = int(math.Ceil(w)), int(math.Ceil(h))
		if format == "png" {
			outW += 100
			outH += 100
		}
	}
	if outW <= 0 || outH <= 0 || int64(outW)*int64(outH) > 100000000 {
		return PageImage{}, fmt.Errorf("page exceeds render size limit")
	}
	bg := ""
	backgroundAlpha := 1.0
	if o.Background != "" && o.Background != "transparent" {
		fill := o.Background
		if len(fill) == 9 && fill[0] == '#' {
			alpha, err := strconv.ParseUint(fill[7:], 16, 8)
			if err != nil {
				return PageImage{}, fmt.Errorf("invalid RGBA background")
			}
			backgroundAlpha = float64(alpha) / 255
			fill = fill[:7]
		}
		bg = fmt.Sprintf(`<rect x="%.4f" y="%.4f" width="%.4f" height="%.4f" fill="%s" opacity="%.6f"/>`, x, y, w, h, html.EscapeString(fill), backgroundAlpha)
	}
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="%d" height="%d" preserveAspectRatio="none" viewBox="%.4f %.4f %.4f %.4f">%s%s%s</svg>`, outW, outH, x, y, w, h, bg, underlay, elements)
	result := PageImage{Width: outW, Height: outH, MIMEType: "image/svg+xml", Data: []byte(svg)}
	if format == "svg" {
		return result, nil
	}
	nativeFitzMu.Lock()
	defer nativeFitzMu.Unlock()
	d, e := fitz.NewFromMemory([]byte(svg))
	if e != nil {
		return PageImage{}, e
	}
	defer d.Close()
	result.Data, e = d.ImagePNG(0, 72)
	if e != nil {
		return PageImage{}, e
	}
	cfg, e := png.DecodeConfig(bytes.NewReader(result.Data))
	if e != nil {
		return PageImage{}, e
	}
	result.Width, result.Height = cfg.Width, cfg.Height
	result.MIMEType = "image/png"
	if (bg == "" || backgroundAlpha < 1) && underlay == "" {
		// The go-fitz API clears its RGBA pixmap to opaque white. Recover
		// true coverage with a second render over black; W-B = 1-alpha.
		// Unlike color-keying, this preserves white ink and antialiasing.
		blackRect := fmt.Sprintf(`<rect x="%.4f" y="%.4f" width="%.4f" height="%.4f" fill="black"/>`, x, y, w, h)
		idx := strings.Index(svg, ">") + 1
		blackSVG := svg[:idx] + blackRect + svg[idx:]
		blackDoc, err := fitz.NewFromMemory([]byte(blackSVG))
		if err != nil {
			return PageImage{}, err
		}
		blackPNG, err := blackDoc.ImagePNG(0, 72)
		blackDoc.Close()
		if err != nil {
			return PageImage{}, err
		}
		white, err := png.Decode(bytes.NewReader(result.Data))
		if err != nil {
			return PageImage{}, err
		}
		black, err := png.Decode(bytes.NewReader(blackPNG))
		if err != nil {
			return PageImage{}, err
		}
		alphaImage := image.NewNRGBA(white.Bounds())
		for py := 0; py < cfg.Height; py++ {
			for px := 0; px < cfg.Width; px++ {
				wr, wg, wb, _ := white.At(px, py).RGBA()
				br, bg, bb, _ := black.At(px, py).RGBA()
				delta := (int(wr) - int(br) + int(wg) - int(bg) + int(wb) - int(bb)) / 3 / 257
				alpha := 255 - delta
				if alpha < 0 {
					alpha = 0
				}
				if alpha > 255 {
					alpha = 255
				}
				c := color.NRGBA{A: uint8(alpha)}
				if alpha > 0 {
					c.R = uint8(math.Min(255, float64(br/257)*255/float64(alpha)))
					c.G = uint8(math.Min(255, float64(bg/257)*255/float64(alpha)))
					c.B = uint8(math.Min(255, float64(bb/257)*255/float64(alpha)))
				}
				alphaImage.SetNRGBA(px, py, c)
			}
		}
		var output bytes.Buffer
		if err := png.Encode(&output, alphaImage); err != nil {
			return PageImage{}, err
		}
		result.Data = output.Bytes()
	}
	return result, nil
}
func ExtractDocument(files map[string][]byte, includeOCR bool) (ExtractedDocument, error) {
	return ExtractDocumentContext(context.Background(), files, includeOCR)
}
func ExtractDocumentContext(ctx context.Context, files map[string][]byte, includeOCR bool) (ExtractedDocument, error) {
	out := ExtractedDocument{}
	if e := ctx.Err(); e != nil {
		return out, e
	}
	a, e := readRenderArchive(files)
	if e != nil {
		return out, e
	}
	out.PageCount = len(a.order)
	source := a.pdf
	if source == nil && len(a.epub) > 0 {
		out.SourceText, e = extractEPUB(ctx, a.epub)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if e != nil {
			// Python's EPUB extractor returns empty text on invalid books.
			// Keep that observable result, but preserve diagnostics for callers.
			out.Warnings = append(out.Warnings, "EPUB extraction: "+e.Error())
		}
	}
	if len(source) > 0 {
		nativeFitzMu.Lock()
		d, e := fitz.NewFromMemory(source)
		if e == nil {
			var chunks []string
			for i := 0; i < d.NumPage(); i++ {
				if err := ctx.Err(); err != nil {
					d.Close()
					nativeFitzMu.Unlock()
					return out, err
				}
				t, err := d.Text(i)
				if err != nil {
					out.Warnings = append(out.Warnings, fmt.Sprintf("source page %d: %v", i+1, err))
					continue
				}
				if t = strings.TrimSpace(t); t != "" {
					if a.pdf != nil {
						t = fmt.Sprintf("--- Page %d ---\n%s", i+1, t)
					}
					chunks = append(chunks, t)
				}
			}
			out.SourceText = strings.Join(chunks, "\n\n")
			if out.PageCount == 0 {
				out.PageCount = d.NumPage()
			}
			d.Close()
		}
		nativeFitzMu.Unlock()
		if e != nil {
			return out, e
		}
	}
	var typed, highlights, annotations []string
	out.PageIDs = append([]string{}, a.order...)
	if includeOCR {
		out.OCRPages = make([]string, len(a.order))
	}
	for i, id := range a.order {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		raw, ok := a.rm[id]
		if !ok {
			continue
		}
		s, e := parseNativeScene(raw)
		if e != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("page %d: %v", i+1, e))
			continue
		}
		for _, t := range s.texts {
			var b strings.Builder
			for _, c := range t.chars {
				b.WriteString(c.text)
			}
			if b.Len() > 0 {
				typed = append(typed, b.String())
				for _, paragraph := range strings.Split(b.String(), "\n") {
					if paragraph = strings.TrimSpace(paragraph); paragraph != "" {
						out.TypedTexts = append(out.TypedTexts, paragraph)
					}
				}
			}
		}
		sort.SliceStable(s.glyphs, func(i, j int) bool {
			a, b := s.glyphs[i], s.glyphs[j]
			if len(a.rects) == 0 || len(b.rects) == 0 {
				return len(a.rects) < len(b.rects)
			}
			if a.rects[0][1] != b.rects[0][1] {
				return a.rects[0][1] < b.rects[0][1]
			}
			return a.rects[0][0] < b.rects[0][0]
		})
		for _, g := range s.glyphs {
			if g.text != "" {
				highlights = append(highlights, g.text)
			}
		}
		ink := false
		for _, b := range s.blocks {
			if l, ok := b.(*rms.SceneLineItemBlock); ok && l.Item.Value != nil && l.Item.DeletedLength == 0 {
				ink = true
				break
			}
		}
		if ink || len(s.glyphs) > 0 {
			annotations = append(annotations, fmt.Sprintf("Page %d: handwriting=%t, highlights=%d", i+1, ink, len(s.glyphs)))
			pageHighlights := []string{}
			for _, g := range s.glyphs {
				if g.text != "" {
					pageHighlights = append(pageHighlights, g.text)
				}
			}
			out.AnnotatedPages = append(out.AnnotatedPages, map[string]any{"page": i + 1, "page_id": id, "has_handwriting": ink, "highlights": pageHighlights})
		}
		if includeOCR && ink {
			merged := false
			img, err := a.render(i+1, RenderOptions{Format: "png", Background: "white", RenderMerged: &merged}, false)
			if err == nil {
				var backend string
				out.OCRPages[i], backend, err = OCRImage(ctx, img.Data)
				if backend != "" {
					out.OCRBackend = backend
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				out.Warnings = append(out.Warnings, fmt.Sprintf("Page %d OCR: %v", i+1, err))
			}
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch path.Ext(name) {
		case ".txt", ".md":
			typed = append(typed, string(files[name]))
			if strings.TrimSpace(string(files[name])) != "" {
				out.TypedTexts = append(out.TypedTexts, string(files[name]))
			}
		case ".content":
			var content struct {
				Text *string `json:"text"`
			}
			if json.Unmarshal(files[name], &content) == nil && content.Text != nil {
				typed = append(typed, *content.Text)
				out.TypedTexts = append(out.TypedTexts, *content.Text)
			}
		case ".highlights":
			var h struct {
				Highlights [][]struct {
					Text string `json:"text"`
				} `json:"highlights"`
			}
			if e := json.Unmarshal(files[name], &h); e != nil {
				out.Warnings = append(out.Warnings, "invalid highlights file: "+name)
			} else {
				for _, group := range h.Highlights {
					for _, v := range group {
						if v.Text != "" {
							highlights = append(highlights, v.Text)
						}
					}
				}
			}
		}
	}
	out.TypedText = strings.Join(typed, "\n")
	out.Highlights = strings.Join(highlights, "\n")
	out.HighlightTexts = append([]string{}, highlights...)
	out.Annotations = strings.Join(annotations, "\n")
	if includeOCR {
		out.OCRText = strings.Join(out.OCRPages, "\n")
	}
	return out, nil
}
func ExportPDF(files map[string][]byte, mode string) ([]byte, ExportInfo, error) {
	return ExportPDFMetadata(files, mode, nil)
}
