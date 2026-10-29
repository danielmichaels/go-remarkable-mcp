package main

import (
	"bytes"
	"fmt"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	htmlrenderer "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Keep this stylesheet aligned with remarkable_mcp.markdown_pdf. Pagination,
// shaping and fallback fonts are handled by MuPDF Story, not a second layout engine.
const markdownCSS = `
body { color: #111; font-family: sans-serif; font-size: 11pt; line-height: 1.45; }
h1 { font-size: 24pt; margin: 0 0 14pt; }
h2 { font-size: 18pt; margin: 18pt 0 10pt; }
h3 { font-size: 14pt; margin: 14pt 0 8pt; }
p, ul, ol, blockquote, pre, table { margin: 0 0 10pt; }
blockquote { border-left: 3px solid #777; color: #444; padding-left: 10pt; }
code, pre { font-family: monospace; }
pre { background-color: #f2f2f2; padding: 8pt; }
table { border-collapse: collapse; width: 100%; }
th, td { border: 1px solid #888; padding: 4pt; text-align: left; }
th { background-color: #e8e8e8; }
`

type safeMarkdownRenderer struct{}

// Retain the image label before Goldmark resolves emphasis, escapes and
// character references. MarkdownIt's image rule receives this raw token text.
type imageTokenParser struct{ parser.InlineParser }

func (p imageTokenParser) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	_, closing := reader.PeekLine()
	n := p.InlineParser.Parse(parent, reader, pc)
	if image, ok := n.(*ast.Image); ok {
		label := reader.Value(text.NewSegment(image.Pos()+2, closing.Start))
		image.SetAttributeString("raw-image-label", append([]byte(nil), label...))
	}
	return n
}

func (safeMarkdownRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(ast.KindImage, func(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			alt := string(n.Text(source))
			if raw, ok := n.AttributeString("raw-image-label"); ok {
				alt = string(raw.([]byte))
			}
			image := n.(*ast.Image)
			if htmlrenderer.IsDangerousURL(image.Destination) {
				_, err := fmt.Fprintf(w, "![%s](%s)", html.EscapeString(alt), html.EscapeString(string(image.Destination)))
				return ast.WalkSkipChildren, err
			}
			if alt == "" {
				alt = "image"
			}
			_, err := fmt.Fprintf(w, "<p><em>[Image omitted: %s]</em></p>", html.EscapeString(alt))
			return ast.WalkSkipChildren, err
		}
		return ast.WalkContinue, nil
	})
}

// MarkdownPDF performs all rendering in-process. Neither HTML nor image URLs
// can load external assets; the native Story is given no resource archive.
func MarkdownPDF(markdown, title string) ([]byte, error) {
	if strings.TrimSpace(markdown) == "" {
		return nil, fmt.Errorf("Markdown content cannot be empty")
	}
	// Disable HTML at the parser, so Markdown inside HTML-looking text is
	// still parsed, just like MarkdownIt's html=False mode.
	blocks := parser.DefaultBlockParsers()
	var safeBlocks []util.PrioritizedValue
	for _, p := range blocks {
		if p.Value == parser.NewHTMLBlockParser() {
			continue
		}
		safeBlocks = append(safeBlocks, p)
	}
	inlines := parser.DefaultInlineParsers()
	var safeInlines []util.PrioritizedValue
	for _, p := range inlines {
		if p.Value == parser.NewRawHTMLParser() {
			continue
		}
		if p.Value == parser.NewLinkParser() {
			p.Value = imageTokenParser{parser.NewLinkParser()}
		}
		safeInlines = append(safeInlines, p)
	}
	md := goldmark.New(goldmark.WithParser(parser.NewParser(
		parser.WithBlockParsers(safeBlocks...), parser.WithInlineParsers(safeInlines...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
	)), goldmark.WithExtensions(extension.Table), goldmark.WithRendererOptions(
		renderer.WithNodeRenderers(util.Prioritized(safeMarkdownRenderer{}, 100)),
	))
	var html bytes.Buffer
	if err := md.Convert([]byte(markdown), &html); err != nil {
		return nil, err
	}
	pdf, err := nativeStoryPDF(html.String(), markdownCSS)
	if err != nil || title == "" {
		return pdf, err
	}
	return nativeVectorExport(pdf, nil, map[string]string{"title": title})
}
