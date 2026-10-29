package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed contracts.json
var contractsJSON []byte

//go:embed canvas.html
var canvasHTML string

type contract struct {
	Tools   []*mcp.Tool   `json:"tools"`
	Prompts []*mcp.Prompt `json:"prompts"`
}

type application struct {
	server          *mcp.Server
	backend         Backend
	transport, root string
	writable        bool
	mu              sync.Mutex
	exports         []savedExport
	exportDir       string
	writeMu         sync.Mutex
	resourceURIs    map[string]bool
}

func newApplication() (*application, error) {
	b, transport, err := SelectBackend()
	if err != nil {
		return nil, err
	}
	return applicationFor(b, transport)
}

func applicationFor(b Backend, transport string) (*application, error) {
	a := &application{backend: b, transport: transport, root: cleanPath(os.Getenv("REMARKABLE_ROOT_PATH")), writable: !Enabled("REMARKABLE_READ_ONLY") && transport != "local-dir"}
	a.server = mcp.NewServer(&mcp.Implementation{Name: "remarkable-go", Version: "0.3.0"}, &mcp.ServerOptions{CompletionHandler: a.complete, InitializedHandler: func(ctx context.Context, _ *mcp.InitializedRequest) { go func() { a.library(ctx) }() }})
	var contracts map[string]contract
	if err := json.Unmarshal(contractsJSON, &contracts); err != nil {
		return nil, err
	}
	mode := transport
	if !a.writable {
		mode = "read-only"
	}
	c, ok := contracts[mode]
	if !ok {
		return nil, fmt.Errorf("unknown transport %q", transport)
	}
	if a.writable && transport == "cloud" {
		c.Tools = append(c.Tools, &mcp.Tool{Name: "remarkable_overwrite_pdf", Description: "Replace a cloud PDF by exact UUID. Requires the same page count. Preserves title, folder, page IDs and annotations. Inspect an uncertain write before retrying.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"document_id": map[string]any{"type": "string"}, "file_path": map[string]any{"type": "string"}}, "required": []string{"document_id", "file_path"}}, Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true)}})
	}
	for _, t := range c.Tools {
		var schema jsonschema.Schema
		data, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &schema); err != nil {
			return nil, err
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return nil, err
		}
		name := t.Name
		hasOutputSchema := t.OutputSchema != nil
		a.server.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, err
				}
			}
			if err := resolved.Validate(args); err != nil {
				return nil, err
			}
			for k, s := range schema.Properties {
				if _, exists := args[k]; !exists && s.Default != nil {
					var v any
					if json.Unmarshal(s.Default, &v) == nil {
						args[k] = v
					}
				}
			}
			var result *mcp.CallToolResult
			if name == "remarkable_delete" && !Enabled("REMARKABLE_SKIP_CONFIRM") {
				message := fmt.Sprintf("Delete %q from reMarkable? This moves it to Trash.", str(args, "document"))
				if boolean(args, "permanent") {
					message = fmt.Sprintf("Permanently delete %q? This cannot be recovered from Trash.", str(args, "document"))
				}
				params := &mcp.ElicitParams{Message: message, RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{"confirm": map[string]any{"type": "boolean", "title": "Confirm deletion", "default": false}}, "required": []string{"confirm"}}}
				var confirmation *mcp.ElicitResult
				var e error
				init := req.Session.InitializeParams()
				if init.ProtocolVersion >= "2026-07-28" {
					if init.Capabilities == nil || init.Capabilities.Elicitation == nil {
						e = errors.New("client cannot confirm deletion")
					} else {
						confirmation, _ = req.Params.InputResponses["confirm"].(*mcp.ElicitResult)
						if confirmation == nil {
							return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"confirm": params}}, nil
						}
					}
				} else {
					confirmation, e = req.Session.Elicit(ctx, params)
				}
				if e != nil {
					result = failure("confirmation_unavailable", "Deletion refused: confirmation is unavailable.", "Use a client with MCP elicitation, or explicitly set REMARKABLE_SKIP_CONFIRM=1 for unattended deletes.")
				} else if confirmation.Action != "accept" || confirmation.Content["confirm"] != true {
					result = response(map[string]any{"deleted": false, "cancelled": true, "document": str(args, "document")}, "Deletion cancelled.")
				}
			}
			var err error
			if result == nil {
				result, err = a.call(ctx, name, args)
			}
			if err != nil {
				result = failure(strings.TrimPrefix(name, "remarkable_")+"_failed", err.Error(), "Check remarkable_status() and inspect uncertain writes before retrying.")
			}
			if hasOutputSchema && len(result.Content) > 0 {
				if text, ok := result.Content[0].(*mcp.TextContent); ok {
					result.StructuredContent = map[string]any{"result": text.Text}
				}
			}
			return result, nil
		})
	}
	for _, p := range c.Prompts {
		a.server.AddPrompt(p, promptHandler(p.Name))
	}
	a.server.AddResource(&mcp.Resource{URI: "ui://remarkable/canvas", Name: "reMarkable Canvas", MIMEType: "text/html;profile=mcp-app"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "ui://remarkable/canvas", MIMEType: "text/html;profile=mcp-app", Text: canvasHTML}}}, nil
	})
	for _, spec := range []struct{ scheme, suffix, mime string }{{"remarkable", ".txt", "text/plain"}, {"remarkableraw", ".txt", "text/plain"}, {"remarkableimg", ".page-{page}.png", "image/png"}, {"remarkableimg", ".page-{page}.merged.png", "image/png"}, {"remarkablesvg", ".page-{page}.svg", "image/svg+xml"}} {
		a.server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: spec.scheme + ":///{path}" + spec.suffix, Name: spec.scheme, MIMEType: spec.mime}, a.readResource)
	}
	return a, nil
}

func cleanPath(s string) string { return path.Clean("/" + strings.TrimSpace(s)) }
func (a *application) library(ctx context.Context) ([]Document, map[string]string, error) {
	docs, err := a.backend.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]Document{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	paths := map[string]string{}
	var full func(string, map[string]bool) (string, bool)
	full = func(id string, seen map[string]bool) (string, bool) {
		if id == "" {
			return "", true
		}
		if id == "trash" || seen[id] {
			return "", false
		}
		d, ok := byID[id]
		if !ok {
			return "", false
		}
		seen[id] = true
		parent, ok := full(d.Parent, seen)
		if !ok {
			return "", false
		}
		return parent + "/" + d.Name, true
	}
	filtered := []Document{}
	for _, d := range docs {
		p, ok := full(d.ID, map[string]bool{})
		if !ok {
			continue
		}
		lower, root := strings.ToLower(p), strings.ToLower(a.root)
		if a.root != "/" && lower != root && !strings.HasPrefix(lower, root+"/") {
			continue
		}
		if a.root != "/" {
			p = p[len(a.root):]
			if p == "" {
				p = "/"
			}
		}
		paths[d.ID] = p
		filtered = append(filtered, d)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return strings.ToLower(filtered[i].Name) < strings.ToLower(filtered[j].Name) })
	a.registerDocumentResources(filtered, paths)
	return filtered, paths, nil
}

func resolve(docs []Document, paths map[string]string, name string) (Document, error) {
	var matches []Document
	for _, d := range docs {
		if d.ID == name {
			return d, nil
		}
		if strings.EqualFold(paths[d.ID], cleanPath(name)) || (!strings.Contains(name, "/") && strings.EqualFold(d.Name, name)) {
			matches = append(matches, d)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Document{}, fmt.Errorf("ambiguous document %q; use its full path or UUID", name)
	}
	return Document{}, fmt.Errorf("document_not_found: %q", name)
}

func (a *application) folder(docs []Document, paths map[string]string, s string) (string, error) {
	if cleanPath(s) == "/" && a.root == "/" {
		return "", nil
	}
	d, e := resolve(docs, paths, cleanPath(s))
	if e != nil {
		return "", e
	}
	if d.Type != "CollectionType" {
		return "", errors.New("destination is not a folder")
	}
	return d.ID, nil
}

func response(v map[string]any, hint string) *mcp.CallToolResult {
	v["_hint"] = hint
	b, _ := json.MarshalIndent(v, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}
func failure(kind, message, suggestion string) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(map[string]any{"_error": map[string]any{"type": kind, "message": message, "suggestion": suggestion}}, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}
func str(v map[string]any, k string) string   { s, _ := v[k].(string); return s }
func boolean(v map[string]any, k string) bool { b, _ := v[k].(bool); return b }
func integer(v map[string]any, k string) int  { f, _ := v[k].(float64); return int(f) }
func tagsMatch(d Document, v any) bool {
	tags, _ := v.([]any)
	if len(tags) == 0 {
		return true
	}
	for _, q := range tags {
		for _, t := range d.Tags {
			if strings.EqualFold(t, fmt.Sprint(q)) {
				return true
			}
		}
	}
	return false
}
func clamp(v, lo, hi int) int         { return max(lo, min(v, hi)) }
func truncate(s string, n int) string { r := []rune(s); return string(r[:min(len(r), n)]) }
func (a *application) modified(d Document) any {
	if d.Modified == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, d.Modified)
	if err != nil {
		return d.Modified
	}
	format := "2006-01-02T15:04:05"
	if t.Nanosecond() != 0 {
		format += ".000000"
	}
	if a.transport == "usb-web" {
		format += "-07:00"
	}
	return t.Format(format)
}
func (a *application) docSummary(d Document, p string) map[string]any {
	result := map[string]any{"name": d.Name, "path": p, "modified": a.modified(d)}
	if len(d.Tags) > 0 {
		result["tags"] = d.Tags
	}
	return result
}

func (a *application) call(ctx context.Context, name string, v map[string]any) (*mcp.CallToolResult, error) {
	if name == "remarkable_refresh" {
		a.writeMu.Lock()
		defer a.writeMu.Unlock()
		b, ok := a.backend.(*SSHBackend)
		if !ok {
			return nil, errors.New("refresh requires SSH")
		}
		err := b.RefreshTablet(ctx)
		return response(map[string]any{"refreshed": err == nil, "refresh_pending": b.RefreshPending()}, "Tablet refresh requested."), err
	}
	docs, paths, err := a.library(ctx)
	if name == "remarkable_status" {
		count := 0
		for _, d := range docs {
			if d.Type != "CollectionType" {
				count++
			}
		}
		matrix := map[string]map[string]bool{}
		for _, transport := range []string{"cloud", "ssh", "usb-web", "local-dir"} {
			caps := map[string]bool{"read": true, "render": true, "export": true, "upload": transport != "local-dir"}
			for _, operation := range []string{"mkdir", "move", "rename", "delete"} {
				caps[operation] = transport == "cloud" || transport == "ssh"
			}
			matrix[transport] = caps
		}
		caps := map[string]bool{}
		for operation, supported := range matrix[a.transport] {
			caps[operation] = supported && (operation == "read" || operation == "render" || operation == "export" || a.writable)
		}
		connection := "file (~/.rmapi)"
		if os.Getenv("REMARKABLE_TOKEN") != "" {
			connection = "environment variable"
		}
		switch b := a.backend.(type) {
		case *LocalDirBackend:
			connection = "local directory at " + b.Dir
		case *SSHBackend:
			connection = fmt.Sprintf("SSH to %s@%s:%d", b.User, b.Host, b.Port)
		case *USBWebBackend:
			connection = "USB web interface at " + b.Host
		}
		selected := a.transport
		if a.transport == "cloud" {
			switch {
			case Enabled("REMARKABLE_USE_LOCAL_DIR") || os.Getenv("REMARKABLE_LOCAL_DIR") != "":
				selected = "local-dir"
			case Enabled("REMARKABLE_USE_USB_WEB"):
				selected = "usb-web"
			case Enabled("REMARKABLE_USE_SSH"):
				selected = "ssh"
			}
		}
		if selected != a.transport {
			connection = fmt.Sprintf("cloud (fell back from %s: transport unavailable, cloud token configured)", selected)
		}
		result := map[string]any{"authenticated": err == nil, "transport": a.transport, "connection": connection, "write_enabled": a.writable, "capabilities_by_transport": matrix}
		if err != nil {
			result["error"] = err.Error()
			return response(result, "Connection failed. Check transport configuration and remarkable_status()."), nil
		}
		result["status"] = "connected"
		result["document_count"] = count
		result["capabilities"] = caps
		if a.root != "/" {
			result["root_path"] = a.root
		}
		if selected != a.transport {
			result["fell_back_to_cloud"] = true
		}
		if b, ok := a.backend.(*SSHBackend); ok {
			result["reliability"] = b.Diagnostics()
		}
		return response(result, "Connection ready."), nil
	}
	if err != nil {
		return nil, err
	}
	switch name {
	case "remarkable_browse":
		items := []map[string]any{}
		query := str(v, "query")
		if query != "" {
			for _, d := range docs {
				if strings.Contains(strings.ToLower(d.Name), strings.ToLower(query)) && tagsMatch(d, v["tags"]) {
					item := a.docSummary(d, paths[d.ID])
					item["type"] = "document"
					if d.Type == "CollectionType" {
						item["type"] = "folder"
					}
					items = append(items, item)
				}
			}
			sort.SliceStable(items, func(i, j int) bool { return items[i]["name"].(string) < items[j]["name"].(string) })
			result := map[string]any{"mode": "search", "query": query, "results": items, "count": len(items)}
			if tags, ok := v["tags"].([]any); ok && len(tags) > 0 {
				result["filter_tags"] = tags
			}
			return response(result, "Use remarkable_read() with a document path."), nil
		}
		folder := str(v, "path")
		if d, e := resolve(docs, paths, folder); e == nil && d.Type != "CollectionType" {
			files, e := a.backend.Download(ctx, d.ID)
			if e != nil {
				return nil, e
			}
			result, e := a.read(ctx, d, paths[d.ID], files, map[string]any{"page": float64(1), "content_type": "text"})
			if e == nil {
				var data map[string]any
				json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &data)
				if data["_error"] == nil {
					data["_redirected_from"] = "browse:" + folder
					result = response(data, "Auto-redirected from browse to read. "+fmt.Sprint(data["_hint"]))
				}
			}
			return result, e
		}
		parent, err := a.folder(docs, paths, folder)
		if err != nil {
			return failure("folder_not_found", fmt.Sprintf("Folder not found: '%s'", folder), "Use remarkable_browse('/') to see root folder contents."), nil
		}
		folders, documents := []map[string]any{}, []map[string]any{}
		for _, d := range docs {
			if d.Parent != parent {
				continue
			}
			if d.Type == "CollectionType" {
				folders = append(folders, map[string]any{"name": d.Name, "id": d.ID})
			} else if tagsMatch(d, v["tags"]) {
				item := a.docSummary(d, paths[d.ID])
				delete(item, "path")
				item["id"] = d.ID
				documents = append(documents, item)
			}
		}
		result := map[string]any{"mode": "browse", "path": folder, "folders": folders, "documents": documents}
		if tags, ok := v["tags"].([]any); ok && len(tags) > 0 {
			result["filter_tags"] = tags
		}
		return response(result, "Use remarkable_browse(path=...) or remarkable_read(document=...)."), nil
	case "remarkable_recent", "remarkable_search":
		selected := []Document{}
		for _, d := range docs {
			if d.Type != "CollectionType" && tagsMatch(d, v["tags"]) && (name == "remarkable_recent" || strings.Contains(strings.ToLower(d.Name), strings.ToLower(str(v, "query")))) {
				selected = append(selected, d)
			}
		}
		if name == "remarkable_recent" {
			sort.SliceStable(selected, func(i, j int) bool {
				ti, _ := time.Parse(time.RFC3339Nano, selected[i].Modified)
				tj, _ := time.Parse(time.RFC3339Nano, selected[j].Modified)
				return ti.After(tj)
			})
		} else {
			sort.SliceStable(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
		}
		limit := clamp(integer(v, "limit"), 1, 50)
		if name == "remarkable_search" {
			limit = min(limit, 5)
		}
		if boolean(v, "include_preview") {
			limit = min(limit, 10)
		}
		items := []map[string]any{}
		if len(selected) == 0 && name == "remarkable_search" {
			return failure("no_documents_found", "No documents found matching the query.", "Use remarkable_browse('/') to list documents."), nil
		}
		for _, d := range selected[:min(limit, len(selected))] {
			item := a.docSummary(d, paths[d.ID])
			if name == "remarkable_recent" && boolean(v, "include_preview") && d.FileType == "notebook" {
				item["preview_skipped"] = "notebook (use remarkable_read with include_ocr)"
				items = append(items, item)
				continue
			}
			if name == "remarkable_search" || boolean(v, "include_preview") {
				files, e := a.backend.Download(ctx, d.ID)
				if e != nil {
					if name == "remarkable_search" {
						item["error"] = e.Error()
					}
				} else {
					if name == "remarkable_search" {
						readResult, readErr := a.read(ctx, d, paths[d.ID], files, map[string]any{"page": float64(1), "content_type": "text", "grep": v["grep"], "include_ocr": v["include_ocr"]})
						if readErr != nil {
							item["error"] = readErr.Error()
						} else {
							var data map[string]any
							json.Unmarshal([]byte(readResult.Content[0].(*mcp.TextContent).Text), &data)
							if errorData, ok := data["_error"].(map[string]any); ok {
								item["error"] = errorData["message"]
							} else {
								content, _ := data["content"].(string)
								item["content"] = truncate(content, 2000)
								for _, k := range []string{"total_pages", "total_pages_known", "content_pages"} {
									item[k] = data[k]
								}
								if str(v, "grep") != "" {
									item["grep_matches"] = data["grep_matches"]
								}
								if len([]rune(content)) > 2000 {
									item["truncated"] = true
								}
							}
						}
						items = append(items, item)
						continue
					}
					x, e := ExtractDocumentContext(ctx, files, false)
					if e == nil && x.TypedText != "" {
						item["preview"] = truncate(x.TypedText, 200)
						if len([]rune(x.TypedText)) >= 200 {
							item["preview"] = item["preview"].(string) + "..."
						}
					}
				}
			}
			items = append(items, item)
		}
		result := map[string]any{"documents": items, "count": len(items)}
		if name == "remarkable_search" {
			result["query"] = str(v, "query")
			result["grep"] = v["grep"]
			if tags, ok := v["tags"].([]any); ok && len(tags) > 0 {
				result["filter_tags"] = tags
			}
		}
		return response(result, "Use remarkable_read() for full content."), nil
	case "remarkable_read", "remarkable_image", "remarkable_export", "remarkable_canvas":
		d, e := resolve(docs, paths, str(v, "document"))
		if e != nil {
			return failure("document_not_found", e.Error(), "Use remarkable_browse() to find a full path."), nil
		}
		if d.Type == "CollectionType" {
			return nil, errors.New("select a document, not a folder")
		}
		files, e := a.backend.Download(ctx, d.ID)
		if e != nil {
			return nil, e
		}
		switch name {
		case "remarkable_read":
			return a.read(ctx, d, paths[d.ID], files, v)
		case "remarkable_image", "remarkable_canvas":
			return a.image(ctx, d, paths[d.ID], files, v, name == "remarkable_canvas")
		default:
			return a.export(ctx, d, paths[d.ID], files, v)
		}
	default:
		if !a.writable {
			return nil, errors.New("write mode disabled")
		}
		a.writeMu.Lock()
		defer a.writeMu.Unlock()
		docs, paths, err = a.library(ctx)
		if err != nil {
			return nil, err
		}
		return a.write(ctx, name, v, docs, paths)
	}
}

func (a *application) close() {
	if b, ok := a.backend.(*SSHBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := b.Close(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "reMarkable SSH shutdown: %v\n", err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.exportDir != "" {
		os.RemoveAll(a.exportDir)
	}
}
