package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/dlclark/regexp2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func documentText(x ExtractedDocument, kind string) string {
	if kind == "raw" {
		return x.SourceText
	}
	parts := []string{}
	if kind == "text" && x.SourceText != "" {
		parts = append(parts, x.SourceText)
	}
	annotations := []string{}
	annotations = append(annotations, x.TypedTexts...)
	shown := map[string]bool{}
	if len(x.AnnotatedPages) > 0 {
		annotations = append(annotations, "\n--- Annotated pages ---")
		for _, page := range x.AnnotatedPages {
			marks := []string{}
			if page["has_handwriting"] == true {
				marks = append(marks, "handwritten notes")
			}
			highlights, _ := page["highlights"].([]string)
			if len(highlights) > 0 {
				suffix := ""
				if len(highlights) != 1 {
					suffix = "s"
				}
				marks = append(marks, fmt.Sprintf("%d highlight%s", len(highlights), suffix))
			}
			if len(marks) == 0 {
				marks = append(marks, "annotated")
			}
			annotations = append(annotations, fmt.Sprintf("Page %v: %s", page["page"], strings.Join(marks, ", ")))
			for _, h := range highlights {
				annotations = append(annotations, "  • "+h)
				shown[h] = true
			}
		}
	}
	others := []string{}
	for _, h := range x.HighlightTexts {
		if h != "" && !shown[h] {
			others = append(others, h)
		}
	}
	if len(others) > 0 {
		annotations = append(annotations, "\n--- Highlights ---")
		annotations = append(annotations, others...)
	}
	if x.OCRText != "" {
		annotations = append(annotations, "\n--- Handwritten (OCR) ---", x.OCRText)
	}
	if len(annotations) > 0 {
		if len(parts) > 0 {
			parts = append(parts, "\n\n=== Annotations ===\n")
		}
		parts = append(parts, annotations...)
	}
	return strings.Join(parts, "\n\n")
}

func compileGrep(pattern string) (*regexp2.Regexp, error) {
	var translated strings.Builder
	inClass := false
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '\\' && i+1 < len(pattern) {
			if pattern[i+1] == 'Z' && !inClass {
				translated.WriteString(`\z`)
			} else {
				translated.WriteString(pattern[i : i+2])
			}
			i++
			continue
		}
		if !inClass && strings.HasPrefix(pattern[i:], "(?P<") {
			translated.WriteString("(?<")
			i += 3
			continue
		}
		if !inClass && strings.HasPrefix(pattern[i:], "(?P=") {
			if end := strings.IndexByte(pattern[i+4:], ')'); end >= 0 {
				translated.WriteString(`\k<` + pattern[i+4:i+4+end] + ">")
				i += 4 + end
				continue
			}
		}
		if pattern[i] == '[' {
			inClass = true
		} else if pattern[i] == ']' {
			inClass = false
		}
		translated.WriteByte(pattern[i])
	}
	re, err := regexp2.Compile(translated.String(), regexp2.IgnoreCase|regexp2.Multiline)
	if err == nil {
		re.MatchTimeout = 2 * time.Second
	}
	return re, err
}
func grepText(s, pattern string) (string, int, error) {
	re, err := compileGrep(pattern)
	if err != nil {
		return "", 0, err
	}
	parts := []string{}
	runes := []rune(s)
	m, err := re.FindRunesMatch(runes)
	for ; m != nil && err == nil; m, err = re.FindNextMatch(m) {
		start, end := max(0, m.Index-100), min(len(runes), m.Index+m.Length+100)
		part := string(runes[start:end])
		if start > 0 {
			part = "..." + part
		}
		if end < len(runes) {
			part += "..."
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "\n\n---\n\n"), len(parts), err
}

func (a *application) read(ctx context.Context, d Document, p string, files map[string][]byte, v map[string]any) (*mcp.CallToolResult, error) {
	page := max(1, integer(v, "page"))
	x, err := ExtractDocumentContext(ctx, files, boolean(v, "include_ocr"))
	if err != nil {
		return nil, err
	}
	kind := str(v, "content_type")
	text := documentText(x, kind)
	ft := DocumentFileType(files)
	if kind == "annotations" && flattenedPDF(files) {
		return failure("annotations_not_available", "Annotations are unavailable because this transport returned a native PDF instead of a document archive.", "Use content_type='raw' or 'text', or connect through cloud/SSH or newer USB firmware that supports rmdoc."), nil
	}
	if kind == "raw" && (ft == "pdf" || ft == "epub") && x.SourceText == "" {
		hasSource := false
		for filename := range files {
			if strings.HasSuffix(filename, "."+ft) {
				hasSource = true
			}
		}
		if !hasSource {
			return failure("raw_not_available", fmt.Sprintf("No raw %s source file found for this document", strings.ToUpper(ft)), "Use content_type='text' for extracted content, or content_type='annotations' for handwritten notes."), nil
		}
	}
	if ft == "notebook" && kind != "raw" && len(x.OCRPages) > 0 {
		pages := []string{}
		for _, text := range x.OCRPages {
			if strings.TrimSpace(text) != "" {
				pages = append(pages, text)
			}
		}
		if len(pages) > 0 {
			if page > len(pages) {
				return failure("page_out_of_range", fmt.Sprintf("Notebook has %d extracted content page(s).", len(pages)), "Use page=1."), nil
			}
			text := pages[page-1]
			matches := 0
			if pattern := str(v, "grep"); pattern != "" {
				re, e := compileGrep(pattern)
				if e != nil {
					return failure("invalid_grep", e.Error(), "Use a valid regular expression."), nil
				}
				_, matches, e = grepText(text, pattern)
				if e != nil {
					return failure("invalid_grep", e.Error(), "Use a valid regex pattern or simple text string."), nil
				}
				if matches == 0 {
					matching := []int{}
					for i, pg := range pages {
						hit, e := re.MatchString(pg)
						if e != nil {
							return nil, e
						}
						if hit {
							matching = append(matching, i+1)
						}
					}
					if len(matching) == 0 {
						return failure("no_grep_matches", fmt.Sprintf("No matches for '%s' in document.", pattern), "Try a different search term."), nil
					}
					return failure("no_match_on_page", fmt.Sprintf("No match for '%s' on page %d.", pattern, page), fmt.Sprintf("Matches found on page(s): %v. Try remarkable_read('%s', page=%d, grep='%s').", matching, d.Name, matching[0], pattern)), nil
				}
			}
			result := map[string]any{"document": d.Name, "path": p, "file_type": ft, "content_type": kind, "content": text, "page": page, "total_pages": max(1, x.PageCount), "total_pages_known": true, "content_pages": len(pages), "page_type": "notebook", "total_chars": len([]rune(text)), "more": page < len(pages), "modified": a.modified(d)}
			if x.OCRBackend != "" {
				result["ocr_backend"] = x.OCRBackend
			}
			if page < len(pages) {
				result["next_page"] = page + 1
			}
			if str(v, "grep") != "" {
				result["grep"] = str(v, "grep")
				result["grep_matches"] = matches
			}
			return response(result, "Notebook OCR is paginated by extracted page."), nil
		}
	}
	matches := 0
	if str(v, "grep") != "" {
		text, matches, err = grepText(text, str(v, "grep"))
		if err != nil {
			return failure("invalid_grep", err.Error(), "Use a valid regex pattern or simple text string."), nil
		}
	}
	runes := []rune(text)
	total := len(runes)
	if total == 0 && ft != "pdf" && ft != "epub" && !boolean(v, "include_ocr") {
		args := map[string]any{}
		for key, value := range v {
			args[key] = value
		}
		args["include_ocr"] = true
		result, e := a.read(ctx, d, p, files, args)
		if e != nil {
			return nil, e
		}
		var data map[string]any
		json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &data)
		if data["_error"] == nil {
			data["_ocr_auto_enabled"] = true
			result = response(data, "OCR auto-enabled (notebook had no typed text). "+fmt.Sprint(data["_hint"]))
		}
		return result, nil
	}
	pages := max(1, (total+7999)/8000)
	if page > pages {
		return failure("page_out_of_range", fmt.Sprintf("Extracted content has %d page(s).", pages), "Use page=1."), nil
	}
	more := page < pages
	result := map[string]any{"document": d.Name, "path": p, "file_type": ft, "content_type": kind, "content": string(runes[min((page-1)*8000, total):min(page*8000, total)]), "page": page, "total_pages": max(1, x.PageCount), "total_pages_known": true, "content_pages": pages, "total_chars": total, "more": more, "modified": a.modified(d)}
	if more {
		result["next_page"] = page + 1
	}
	if len(d.Tags) > 0 {
		result["tags"] = d.Tags
	}
	if len(x.Warnings) > 0 {
		result["warnings"] = x.Warnings
	}
	if x.OCRBackend != "" {
		result["ocr_backend"] = x.OCRBackend
	}
	if str(v, "grep") != "" {
		result["grep"] = str(v, "grep")
		result["grep_matches"] = matches
	}
	return response(result, fmt.Sprintf("Content page %d/%d; document has %d physical page(s).", page, pages, x.PageCount)), nil
}

func flattenedPDF(files map[string][]byte) bool {
	if len(files) != 1 {
		return false
	}
	for filename := range files {
		return strings.HasSuffix(filename, ".pdf")
	}
	return false
}

func (a *application) image(ctx context.Context, d Document, p string, files map[string][]byte, v map[string]any, canvas bool) (*mcp.CallToolResult, error) {
	page := integer(v, "page")
	format := strings.ToLower(str(v, "output_format"))
	if canvas {
		format = "png"
	}
	if format != "png" && format != "svg" {
		return failure("invalid_format", fmt.Sprintf("Invalid format: '%s'. Supported formats: png, svg", format), "Use output_format='png' for raster or 'svg' for vectors."), nil
	}
	total, err := DocumentPageCount(files)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return failure("no_pages", fmt.Sprintf("Document '%s' has no renderable pages.", d.Name), "Use remarkable_read() to extract text content instead."), nil
	}
	if page < 1 || page > total {
		return failure("page_out_of_range", fmt.Sprintf("Page %d does not exist. Document has %d page(s).", page, total), fmt.Sprintf("Use page=1 to %d to view different pages.", total)), nil
	}
	if flattenedPDF(files) && !canvas {
		if format == "svg" {
			return failure("svg_not_available", "SVG output is unavailable because this transport returned a native PDF instead of a document archive.", "Retry with output_format='png'."), nil
		}
		if value, ok := v["render_merged"].(bool); ok && !value {
			return failure("annotation_only_not_available", "Annotation-only rendering is unavailable because this transport returned a native PDF instead of a document archive.", "Use the default PNG render, or connect through cloud/SSH or newer USB firmware that supports rmdoc."), nil
		}
	}
	var merged *bool
	if value, ok := v["render_merged"].(bool); ok {
		merged = &value
	}
	if format == "svg" {
		merged = new(false)
	}
	var img PageImage
	var paper [2]float64
	var source string
	if canvas {
		img, paper, source, err = RenderCanvasPage(files, page)
	} else {
		img, err = RenderPage(files, page, RenderOptions{Background: str(v, "background"), Format: format, RenderMerged: merged})
	}
	if err != nil {
		return nil, err
	}
	b64 := base64.StdEncoding.EncodeToString(img.Data)
	resourcePath := (&url.URL{Path: strings.TrimPrefix(p, "/")}).EscapedPath()
	uri := fmt.Sprintf("remarkableimg:///%s.page-%d.png", resourcePath, page)
	if format == "svg" {
		uri = fmt.Sprintf("remarkablesvg:///%s.page-%d.svg", resourcePath, page)
	}
	details := map[string]any{"page": page, "total_pages": total}
	archive, e := readRenderArchive(files)
	if e != nil {
		return nil, e
	}
	isMerged := !canvas && !flattenedPDF(files) && format == "png" && len(archive.pdf) > 0 && (merged == nil || *merged) && archive.pdfIndex(page) >= 0
	details["merged"] = isMerged
	if isMerged {
		uri = strings.TrimSuffix(uri, ".png") + ".merged.png"
	}
	details["mime_type"] = img.MIMEType
	details["resource_uri"] = uri
	if format == "png" {
		details["render_source"] = "strokes"
		if flattenedPDF(files) {
			details["render_source"] = "tablet_pdf"
		}
	}
	if boolean(v, "include_ocr") && format == "png" {
		text, backend, e := OCRImage(ctx, img.Data)
		if e != nil || strings.TrimSpace(text) == "" {
			details["ocr_text"] = nil
			details["ocr_backend"] = nil
			details["ocr_message"] = "No text detected in image"
		} else {
			details["ocr_text"] = text
			details["ocr_backend"] = backend
		}
	}
	if boolean(v, "compatibility") {
		if format == "svg" {
			details["svg"] = string(img.Data)
		} else {
			details["image_base64"] = b64
			details["data_uri"] = "data:" + img.MIMEType + ";base64," + b64
		}
		return response(details, "Page rendered."), nil
	}
	infoText := fmt.Sprintf("Page %d/%d of '%s' as %s. Resource URI: %s", page, total, d.Name, strings.ToUpper(format), uri)
	if isMerged {
		infoText += "\nRendered with PDF + annotation compositing."
	}
	if flattenedPDF(files) {
		infoText += "\nRendered via the tablet's native PDF export (local stroke render unavailable)."
	}
	if text, ok := details["ocr_text"].(string); ok {
		infoText += fmt.Sprintf("\n\nOCR Text (via %s):\n%s", details["ocr_backend"], text)
	} else if boolean(v, "include_ocr") && format == "png" {
		infoText += "\n\nOCR: No text detected in image."
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: infoText}}}
	resource := &mcp.ResourceContents{URI: uri, MIMEType: img.MIMEType}
	if format == "svg" {
		resource.Text = string(img.Data)
	} else {
		resource.Blob = img.Data
	}
	result.Content = append(result.Content, &mcp.EmbeddedResource{Resource: resource})
	if canvas {
		result.Content[0] = response(details, "Page rendered.").Content[0]
		result.StructuredContent = map[string]any{"document": p, "document_name": d.Name, "page": page, "total_pages": total, "png_data_uri": "data:image/png;base64," + b64, "page_width_px": img.Width, "page_height_px": img.Height, "paper_size": paper, "render_source": source, "file_type": DocumentFileType(files), "transport": a.transport, "write_mode": a.writable, "writable": a.writable && a.transport == "ssh"}
	}
	return result, nil
}

type savedExport struct {
	uri, path string
	expires   time.Time
}

func (a *application) export(ctx context.Context, d Document, p string, files map[string][]byte, v map[string]any) (*mcp.CallToolResult, error) {
	format := str(v, "output_format")
	mode := str(v, "pdf_mode")
	if format == "markdown" && mode != "merged" {
		return failure("invalid_export_options", "pdf_mode applies only to PDF exports.", "Use pdf_mode='merged' for Markdown, or choose output_format='pdf'."), nil
	}
	if format == "pdf" && boolean(v, "include_ocr") {
		return failure("invalid_export_options", "include_ocr applies only to Markdown exports.", "Set include_ocr=False, or choose output_format='markdown'."), nil
	}
	if format == "pdf" && mode == "annotations" && flattenedPDF(files) {
		return failure("annotation_only_not_available", "Annotation-only PDF export is unavailable because this transport returned a flattened native PDF.", "Use pdf_mode='merged', or connect through cloud/SSH, local-dir, or newer USB firmware that exposes an rmdoc archive."), nil
	}
	mime := "application/pdf"
	var data []byte
	var err error
	info := ExportInfo{Status: "complete", FailedPages: []int{}, Warnings: []string{}}
	sourceType := DocumentFileType(files)
	representation := "document_archive"
	if flattenedPDF(files) {
		representation = "tablet_pdf"
	}
	if format == "markdown" {
		mime = "text/markdown; charset=utf-8"
		x, e := ExtractDocumentContext(ctx, files, boolean(v, "include_ocr"))
		if e != nil {
			return nil, e
		}
		if flattenedPDF(files) {
			x.Warnings = append(x.Warnings, "This transport returned a flattened native PDF. Source/visible text was preserved, but separate typed, annotation, highlight, and OCR layers were unavailable.")
		}
		data = markdownExport(d, p, a.modified(d), sourceType, x, boolean(v, "include_ocr"))
		info.Pages = x.PageCount
		info.Warnings = x.Warnings
		if len(info.Warnings) > 0 {
			info.Status = "partial"
		}
	} else {
		data, info, err = ExportPDFMetadata(files, mode, map[string]string{
			"title":    d.Name,
			"subject":  fmt.Sprintf("reMarkable document ID: %s; path: %s; source type: %s", d.ID, p, sourceType),
			"keywords": strings.Join(append([]string{"remarkable-document-id:" + d.ID}, d.Tags...), ", "),
			"creator":  "remarkable-mcp", "producer": "remarkable-mcp (native MuPDF)",
		})
		if err != nil {
			return nil, err
		}
	}
	if info.FailedPages == nil {
		info.FailedPages = []int{}
	}
	if info.Warnings == nil {
		info.Warnings = []string{}
	}
	ext := "pdf"
	if format == "markdown" {
		ext = "md"
	}
	name := d.Name
	for _, suffix := range []string{".pdf", ".epub"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			name = name[:len(name)-len(suffix)]
			break
		}
	}
	name = strings.Trim(strings.TrimSpace(filepath.Base(name+"."+ext)), ".")
	name = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == ' ' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "remarkable-export." + ext
	}
	if len([]rune(name)) > 160 {
		name = truncate(strings.TrimSuffix(name, "."+ext), 160-len(ext)-1) + "." + ext
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.exportDir == "" {
		a.exportDir, err = os.MkdirTemp("", "remarkable-exports-")
		if err != nil {
			return nil, err
		}
	}
	f, err := os.CreateTemp(a.exportDir, "export-")
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	if err = f.Close(); err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	uri := "remarkableexport:///" + format + "/" + strings.ReplaceAll(NewID(), "-", "")
	expires := time.Now().Add(15 * time.Minute)
	a.exports = append(a.exports, savedExport{uri, f.Name(), expires})
	for len(a.exports) > 8 || time.Now().After(a.exports[0].expires) {
		old := a.exports[0]
		a.exports = a.exports[1:]
		os.Remove(old.path)
		a.server.RemoveResources(old.uri)
	}
	a.server.AddResource(&mcp.Resource{URI: uri, Name: name, MIMEType: mime}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if time.Now().After(expires) {
			return nil, errors.New("export expired; export again")
		}
		b, e := os.ReadFile(f.Name())
		if e != nil {
			return nil, errors.New("export evicted; export again")
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Blob: b}}}, nil
	})
	expiry := expires.UTC().Format("2006-01-02T15:04:05.000000-07:00")
	metadata := map[string]any{"document": d.Name, "document_id": d.ID, "path": p, "source_type": sourceType, "format": format, "mime_type": mime, "filename": name, "size": len(data), "pages": info.Pages, "failed_pages": info.FailedPages, "warnings": info.Warnings, "status": info.Status, "representation": representation, "resource_uri": uri, "expires_at": expiry, "temporary_local_file": true}
	if format == "pdf" {
		metadata["pdf_mode"] = mode
	}
	result := response(metadata, "Temporary export; save before expiry.")
	size := int64(len(data))
	result.Content = append(result.Content, &mcp.ResourceLink{URI: uri, Name: name, MIMEType: mime, Size: &size, Title: d.Name + " (" + strings.ToUpper(format) + " export)", Description: fmt.Sprintf("Temporary export of reMarkable document %s; expires %s.", d.ID, expiry), Meta: mcp.Meta{"documentId": d.ID, "documentPath": p, "expiresAt": expiry, "temporaryLocalFile": true}})
	return result, nil
}

func jsonBytes(v any) []byte { b, _ := json.Marshal(v); return b }
