package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
)

// EPUB extraction follows ebooklib's manifest order (including navigation and
// non-spine XHTML documents), not MuPDF's paginated reading/spine order.
func extractEPUB(ctx context.Context, data []byte) (string, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	read := func(name string) ([]byte, error) {
		f, err := z.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 32<<20+1))
		if len(b) > 32<<20 {
			return nil, fmt.Errorf("EPUB entry exceeds 32 MiB")
		}
		return b, err
	}
	container, err := read("META-INF/container.xml")
	if err != nil {
		return "", err
	}
	var c struct {
		Roots []struct {
			Path string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err = xml.Unmarshal(container, &c); err != nil {
		return "", err
	}
	if len(c.Roots) == 0 {
		return "", fmt.Errorf("EPUB has no rootfile")
	}
	opf, err := read(c.Roots[0].Path)
	if err != nil {
		return "", err
	}
	var pkg struct {
		Items []struct {
			Href       string `xml:"href,attr"`
			Media      string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"manifest>item"`
	}
	if err = xml.Unmarshal(opf, &pkg); err != nil {
		return "", err
	}
	var parts []string
	for _, item := range pkg.Items {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if item.Media != "application/xhtml+xml" {
			continue
		}
		href, err := url.PathUnescape(item.Href)
		if err != nil {
			return "", err
		}
		html, err := read(path.Join(path.Dir(c.Roots[0].Path), href))
		if err != nil {
			return "", err
		}
		properties := strings.Fields(item.Properties)
		if slices.Contains(properties, "cover") && !slices.Contains(properties, "nav") {
			// ebooklib replaces cover XHTML with its image-only template and generated title.
			parts = append(parts, "Cover")
			continue
		}
		text, err := nativeEPUBHTMLText(html)
		if err != nil {
			return "", err
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}
