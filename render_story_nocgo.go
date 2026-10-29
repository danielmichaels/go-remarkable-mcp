//go:build !cgo || nocgo

package main

import "fmt"

func nativeStoryPDF(html, css string) ([]byte, error) {
	return nil, fmt.Errorf("Markdown Story rendering requires a cgo build with MuPDF")
}

func nativeEPUBHTMLText(html []byte) (string, error) {
	return "", fmt.Errorf("EPUB HTML extraction requires a cgo build with MuPDF")
}

func nativeVectorExport(source []byte, pages []vectorExportPage, metadata map[string]string) ([]byte, error) {
	return nil, fmt.Errorf("vector PDF export requires a cgo build with MuPDF")
}
