package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

func DocumentPageCount(files map[string][]byte) (int, error) {
	a, e := readRenderArchive(files)
	if e != nil {
		return 0, e
	}
	return len(a.order), nil
}
func DocumentFileType(files map[string][]byte) string {
	for n, b := range files {
		if path.Ext(n) == ".content" {
			var c struct {
				FileType string `json:"fileType"`
			}
			if json.Unmarshal(b, &c) == nil && c.FileType != "" {
				return c.FileType
			}
		}
	}
	for n := range files {
		if path.Ext(n) == ".epub" {
			return "epub"
		}
	}
	for n := range files {
		if path.Ext(n) == ".pdf" {
			return "pdf"
		}
	}
	return "notebook"
}

// OCRImage invokes the native Tesseract executable, or the existing opt-in
// Google Vision OCR API. It never launches Python or a generic worker process.
func OCRImage(ctx context.Context, data []byte) (string, string, error) {
	if _, _, e := image.DecodeConfig(bytes.NewReader(data)); e != nil {
		return "", "", fmt.Errorf("invalid OCR image: %w", e)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("REMARKABLE_OCR_BACKEND")))
	if backend != "google" && backend != "tesseract" {
		backend = "auto"
	}
	key := os.Getenv("GOOGLE_VISION_API_KEY")
	if backend == "auto" {
		if key != "" {
			backend = "google"
		} else {
			backend = "tesseract"
		}
	}
	var googleErr error
	if backend == "google" {
		var t string
		t, googleErr = googleImageOCR(ctx, data, key)
		if googleErr == nil {
			return t, "google", nil
		}
	}
	cmd := exec.CommandContext(ctx, "tesseract", "stdin", "stdout", "--psm", "6")
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, e := cmd.Output()
	if e != nil {
		if ctx.Err() != nil {
			e = ctx.Err()
		}
		if googleErr != nil {
			return "", "", fmt.Errorf("Google OCR failed; native Tesseract fallback failed: %w", e)
		}
		return "", "", fmt.Errorf("native Tesseract OCR failed: %w", e)
	}
	return strings.TrimSpace(string(out)), "tesseract", nil
}
func googleImageOCR(ctx context.Context, data []byte, key string) (string, error) {
	payload := map[string]any{"requests": []any{map[string]any{"image": map[string]string{"content": base64.StdEncoding.EncodeToString(data)}, "features": []any{map[string]string{"type": "DOCUMENT_TEXT_DETECTION"}}}}}
	body, e := json.Marshal(payload)
	if e != nil {
		return "", e
	}
	endpoint := "https://vision.googleapis.com/v1/images:annotate"
	if key != "" {
		endpoint += "?key=" + url.QueryEscape(key)
	}
	req, e := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if e != nil {
		return "", e
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.DefaultClient
	if key == "" {
		client, e = google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if e != nil {
			return "", fmt.Errorf("Google default credentials unavailable")
		}
	}
	response, e := client.Do(req)
	if e != nil {
		return "", fmt.Errorf("Google Vision request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Google Vision HTTP %d", response.StatusCode)
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if e != nil {
		return "", e
	}
	var result struct {
		Responses []struct {
			FullTextAnnotation struct {
				Text string `json:"text"`
			} `json:"fullTextAnnotation"`
			TextAnnotations []struct {
				Description string `json:"description"`
			} `json:"textAnnotations"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		} `json:"responses"`
	}
	if e = json.Unmarshal(raw, &result); e != nil {
		return "", e
	}
	if len(result.Responses) == 0 {
		return "", fmt.Errorf("Google Vision returned no result")
	}
	r := result.Responses[0]
	if r.Error != nil {
		return "", fmt.Errorf("Google Vision error code %d", r.Error.Code)
	}
	if r.FullTextAnnotation.Text != "" {
		return strings.TrimSpace(r.FullTextAnnotation.Text), nil
	}
	if len(r.TextAnnotations) > 0 {
		return strings.TrimSpace(r.TextAnnotations[0].Description), nil
	}
	return "", nil
}
