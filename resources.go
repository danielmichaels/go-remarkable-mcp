package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var imageResourcePath = regexp.MustCompile(`^(.*)\.page-([0-9]+)(\.merged)?\.(png|svg)$`)

func (a *application) registerDocumentResources(docs []Document, paths map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	next := map[string]bool{}
	for _, d := range docs {
		if d.Type == "CollectionType" {
			continue
		}
		p := (&url.URL{Path: strings.TrimPrefix(paths[d.ID], "/")}).EscapedPath()
		for _, scheme := range []string{"remarkable", "remarkableraw"} {
			if scheme == "remarkableraw" && d.FileType != "pdf" && d.FileType != "epub" {
				continue
			}
			uri := scheme + ":///" + p + ".txt"
			next[uri] = true
			if !a.resourceURIs[uri] {
				a.server.AddResource(&mcp.Resource{URI: uri, Name: d.Name, MIMEType: "text/plain"}, a.readResource)
			}
		}
	}
	for uri := range a.resourceURIs {
		if !next[uri] {
			a.server.RemoveResources(uri)
		}
	}
	a.resourceURIs = next
}

func (a *application) readResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	u, err := url.Parse(req.Params.URI)
	if err != nil {
		return nil, err
	}
	docs, paths, err := a.library(ctx)
	if err != nil {
		return nil, err
	}
	p := u.Path
	page := 1
	format := "png"
	var merged *bool
	if u.Scheme == "remarkableimg" || u.Scheme == "remarkablesvg" {
		m := imageResourcePath.FindStringSubmatch(p)
		if m == nil {
			return nil, errors.New("invalid image resource URI")
		}
		p = m[1]
		page, _ = strconv.Atoi(m[2])
		format = m[4]
		merged = new(m[3] != "")
	} else {
		p = strings.TrimSuffix(p, ".txt")
	}
	d, err := resolve(docs, paths, p)
	if err != nil {
		return nil, err
	}
	files, err := a.backend.Download(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	r := &mcp.ResourceContents{URI: req.Params.URI}
	switch u.Scheme {
	case "remarkable", "remarkableraw":
		x, e := ExtractDocumentContext(ctx, files, false)
		if e != nil {
			return nil, e
		}
		kind := "annotations"
		if u.Scheme == "remarkableraw" {
			kind = "raw"
		}
		r.MIMEType = "text/plain"
		r.Text = documentText(x, kind)
		if r.Text == "" && kind == "annotations" {
			x, e = ExtractDocumentContext(ctx, files, true)
			if e != nil {
				return nil, e
			}
			r.Text = documentText(x, kind)
		}
		if r.Text == "" {
			r.Text = "(No user content)"
		}
	case "remarkableimg", "remarkablesvg":
		if format == "svg" {
			merged = new(false)
		}
		img, e := RenderPage(files, page, RenderOptions{Format: format, RenderMerged: merged})
		if e != nil {
			return nil, e
		}
		r.MIMEType = img.MIMEType
		if format == "svg" {
			r.Text = string(img.Data)
		} else {
			r.Blob = img.Data
		}
	default:
		return nil, errors.New("unknown resource scheme")
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{r}}, nil
}

func (a *application) complete(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	values := []string{}
	docs, paths, err := a.library(ctx)
	if err != nil {
		return nil, err
	}
	if req.Params.Argument.Name == "page" {
		if req.Params.Context != nil {
			p := req.Params.Context.Arguments["path"]
			d, e := resolve(docs, paths, p)
			if e == nil {
				files, e := a.backend.Download(ctx, d.ID)
				if e == nil {
					count, e := DocumentPageCount(files)
					if e == nil {
						for i := 1; i <= min(count, 10); i++ {
							s := strconv.Itoa(i)
							if strings.HasPrefix(s, req.Params.Argument.Value) {
								values = append(values, s)
							}
						}
					}
				}
			}
		}
	} else {
		for _, d := range docs {
			if d.Type == "CollectionType" {
				continue
			}
			p := strings.TrimPrefix(paths[d.ID], "/")
			if strings.Contains(strings.ToLower(p), strings.ToLower(req.Params.Argument.Value)) {
				values = append(values, p)
			}
		}
	}
	sort.Strings(values)
	total := len(values)
	return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: values[:min(total, 100)], Total: total, HasMore: total > 100}}, nil
}

func promptHandler(name string) mcp.PromptHandler {
	return func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		args := req.Params.Arguments
		text := ""
		switch name {
		case "summarize_recent":
			text = "Please check my recent reMarkable documents using remarkable_recent() and provide a summary of what I've been working on. For any documents that look interesting, read their content with remarkable_read() and give me key highlights."
		case "find_notes":
			text = fmt.Sprintf("Search my reMarkable tablet for any notes about '%s'. Use remarkable_browse(query='%s') to find relevant documents, then use remarkable_read() to extract and summarize the content. Please organize the information you find.", args["topic"], args["topic"])
		case "daily_review":
			text = "Please do a daily review of my reMarkable notes:\n\n1. Use remarkable_recent(limit=5, include_preview=True) to see what I worked on recently\n2. For documents modified today, read the full content\n3. Summarize the key points and any action items\n4. Suggest any follow-up tasks based on my notes"
		case "export_document":
			text = fmt.Sprintf("Please export my reMarkable document '%s' as Markdown using remarkable_export('%s', output_format='markdown'). Return the temporary resource link and its expiry so I can save it.", args["document_name"], args["document_name"])
		case "organize_library":
			text = "Please help me organize my reMarkable library:\n\n1. Use remarkable_browse('/') to see my current folder structure\n2. Use remarkable_recent(limit=20) to see my active documents\n3. Identify any documents that might be misplaced or could be better organized\n4. Suggest a folder structure that would help me stay organized\n\nNote: Please just provide recommendations - I'll review them before making any changes on my tablet."
		case "meeting_notes":
			keyword := args["meeting_keyword"]
			if keyword == "" {
				keyword = "meeting"
			}
			text = fmt.Sprintf("Find all my meeting notes on my reMarkable tablet:\n\n1. Search for documents with remarkable_browse(query='%s')\n2. Read the content of each meeting notes document\n3. Extract key decisions, action items, and attendees mentioned\n4. Create a consolidated summary of all meetings", keyword)
		default:
			return nil, errors.New("unknown prompt")
		}
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}, nil
	}
}
