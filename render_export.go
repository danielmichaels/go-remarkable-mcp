package main

import (
	"fmt"
	"html"
)

type vectorExportPage struct {
	svg                string
	index              int
	width, height, ppu float64
}

// ExportPDFMetadata flattens native page drawing operations into a new PDF:
// original PDF text stays selectable and vectors stay vectors. Interactive
// source annotations, outlines and link actions are not copied to the new file.
// Subject and keywords append attribution to the source values, never erase it.
func ExportPDFMetadata(files map[string][]byte, mode string, metadata map[string]string) ([]byte, ExportInfo, error) {
	info := ExportInfo{Status: "complete"}
	if mode != "merged" && mode != "annotations" {
		return nil, info, fmt.Errorf("unsupported PDF export mode %q", mode)
	}
	a, err := readRenderArchive(files)
	if err != nil {
		return nil, info, err
	}
	if len(a.order) == 0 {
		return nil, info, fmt.Errorf("document has no physical pages")
	}
	source := a.pdf
	sourceCount := 0
	var sourceErr error
	if len(source) > 0 {
		sourceCount, sourceErr = PDFPageCount(source)
		if sourceErr != nil {
			source = nil
		}
	}
	w, h := a.paperSize()
	pages := make([]vectorExportPage, 0, len(a.order))
	for i, id := range a.order {
		scene := &nativeScene{w: w, h: h}
		if raw, ok := a.rm[id]; ok {
			scene, err = parseNativeScene(raw)
		}
		index := -1
		if mode == "merged" && len(a.pdf) > 0 {
			index = a.pdfIndex(i + 1)
			if index >= 0 && sourceErr != nil {
				err = sourceErr
			}
			if index >= sourceCount && sourceErr == nil {
				err = fmt.Errorf("mapped PDF page %d is out of range", index)
			}
		}
		var elements string
		if err != nil {
			info.Status = "partial"
			info.FailedPages = append(info.FailedPages, i+1)
			info.Warnings = append(info.Warnings, fmt.Sprintf("Page %d: %v", i+1, err))
			elements = fmt.Sprintf(`<text x="%f" y="60" font-size="24">reMarkable export placeholder: Physical page %d could not be rendered. %s</text>`, -w/2+30, i+1, html.EscapeString(err.Error()))
			scene = &nativeScene{w: w, h: h}
			index = -1
			err = nil
		} else {
			elements, _ = scene.elements()
		}
		ppu := .3177
		if scene.w == 1620 && scene.h == 2160 {
			ppu = .3144
		}
		svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="%.4f" height="%.4f" viewBox="%.4f 0 %.4f %.4f">%s</svg>`, scene.w, scene.h, -scene.w/2, scene.w, scene.h, elements)
		pages = append(pages, vectorExportPage{svg: svg, index: index, width: scene.w, height: scene.h, ppu: ppu})
	}
	info.Pages = len(pages)
	// Retain source metadata even for annotations-only exports.
	result, err := nativeVectorExport(source, pages, metadata)
	return result, info, err
}
