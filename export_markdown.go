package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

func markdownExport(d Document, path string, modified any, sourceType string, x ExtractedDocument, includeOCR bool) []byte {
	value := func(v any) string {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.Encode(v)
		return strings.TrimSpace(b.String())
	}
	var physical any
	if x.PageCount > 0 {
		physical = x.PageCount
	}
	status := "complete"
	if len(x.Warnings) > 0 {
		status = "partial"
	}
	lines := []string{"---", "title: " + value(d.Name), "remarkable_document_id: " + value(d.ID), "remarkable_path: " + value(path), "source_type: " + value(sourceType), "physical_pages: " + value(physical), "modified: " + value(modified), "export_status: " + value(status)}
	if len(d.Tags) > 0 {
		lines = append(lines, "tags:")
		for _, tag := range d.Tags {
			lines = append(lines, "  - "+value(tag))
		}
	} else {
		lines = append(lines, "tags: []")
	}
	title := strings.Join(strings.Fields(d.Name), " ")
	if title == "" {
		title = "Untitled reMarkable document"
	}
	lines = append(lines, "---", "", "# "+title, "", "## Source text", "")
	verbatim := func(text string) {
		longest := 0
		for _, fence := range regexp.MustCompile("`+").FindAllString(text, -1) {
			longest = max(longest, len(fence))
		}
		fence := strings.Repeat("`", max(3, longest+1))
		lines = append(lines, fence+"text", strings.TrimRight(text, " \t\r\n"), fence)
	}
	if sourceType == "notebook" {
		lines = append(lines, "_Not applicable to a native notebook._")
	} else if x.SourceText != "" {
		verbatim(x.SourceText)
	} else {
		lines = append(lines, "_No source text was extracted._")
	}
	lines = append(lines, "", "## Typed text", "")
	typed := []string{}
	for _, text := range x.TypedTexts {
		if strings.TrimSpace(text) != "" {
			typed = append(typed, text)
		}
	}
	if len(typed) > 0 {
		for i, text := range typed {
			if len(typed) > 1 {
				lines = append(lines, fmt.Sprintf("### Typed excerpt %d", i+1), "")
			}
			verbatim(text)
			lines = append(lines, "")
		}
	} else {
		lines = append(lines, "_No typed text was extracted._", "")
	}
	lines = append(lines, "## Annotations", "")
	if len(x.AnnotatedPages) == 0 {
		lines = append(lines, "_No annotated pages were identified._")
	} else {
		for _, entry := range x.AnnotatedPages {
			marks := []string{}
			if entry["has_handwriting"] == true {
				marks = append(marks, "handwriting")
			}
			highlights, _ := entry["highlights"].([]string)
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
			identity := ""
			if id, _ := entry["page_id"].(string); id != "" {
				identity = "; page ID `" + id + "`"
			}
			lines = append(lines, fmt.Sprintf("- Physical page %v%s: %s", entry["page"], identity, strings.Join(marks, ", ")))
		}
	}
	lines = append(lines, "", "## Highlights", "")
	if strings.TrimSpace(x.Highlights) == "" {
		lines = append(lines, "_No highlighted text was extracted._", "")
	} else {
		for i, highlight := range x.HighlightTexts {
			lines = append(lines, fmt.Sprintf("### Highlight %d", i+1), "")
			verbatim(highlight)
			lines = append(lines, "")
		}
	}
	lines = append(lines, "## OCR", "")
	if !includeOCR {
		lines = append(lines, "_OCR was not requested for this export._", "")
	} else {
		if x.OCRBackend != "" {
			lines = append(lines, "Backend: `"+x.OCRBackend+"`", "")
		}
		ocr := []string{}
		for _, s := range x.OCRPages {
			if strings.TrimSpace(s) != "" {
				ocr = append(ocr, s)
			}
		}
		if len(ocr) == 0 {
			lines = append(lines, "_No OCR text was extracted._", "")
		} else {
			lines = append(lines, "_OCR excerpts remain in extraction order. Sparse OCR results are not assigned to physical pages when the extractor cannot prove that mapping._", "")
			for i, s := range ocr {
				lines = append(lines, fmt.Sprintf("### OCR excerpt %d", i+1), "")
				verbatim(s)
				lines = append(lines, "")
			}
		}
	}
	if len(x.Warnings) > 0 {
		lines = append(lines, "## Export notes", "")
		for _, warning := range x.Warnings {
			lines = append(lines, "- "+warning)
		}
		lines = append(lines, "")
	}
	return []byte(strings.TrimRight(strings.Join(lines, "\n"), " \t\r\n") + "\n")
}
