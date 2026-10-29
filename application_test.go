package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectTestApp(t *testing.T, a *application, options ...*mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	ss, e := a.server.Connect(ctx, st, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ss.Close() })
	var opts *mcp.ClientOptions
	if len(options) > 0 {
		opts = options[0]
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "integration", Version: "1"}, opts)
	cs, e := client.Connect(ctx, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cs.Close(); a.close() })
	return cs
}

func callTestTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	result, e := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if e != nil {
		t.Fatal(e)
	}
	if result.IsError {
		t.Fatalf("%s: %+v", name, result)
	}
	var data map[string]any
	if len(result.Content) > 0 {
		if text, ok := result.Content[0].(*mcp.TextContent); ok {
			if name == "remarkable_image" && len(result.Content) > 1 {
				return result, nil
			}
			if e = json.Unmarshal([]byte(text.Text), &data); e != nil {
				t.Fatal(e)
			}
		}
	}
	return result, data
}

func localAppFixture(t *testing.T) *application {
	t.Helper()
	root := t.TempDir()
	id := "00000000-0000-4000-8000-000000000001"
	folder := "00000000-0000-4000-8000-000000000002"
	page, e := NewNotebookPage("Local native content café native native")
	if e != nil {
		t.Fatal(e)
	}
	page, e = AuthorPage(page, []map[string]any{{"points": [][]float64{{0.1, 0.3}, {0.8, 0.3}}, "color": "red", "tool": "fineliner"}})
	if e != nil {
		t.Fatal(e)
	}
	files := map[string][]byte{folder + ".metadata": jsonBytes(map[string]any{"visibleName": "Work", "type": "CollectionType", "parent": ""}), id + ".metadata": jsonBytes(map[string]any{"visibleName": "Notes", "type": "DocumentType", "parent": folder, "lastModified": "1700000000000", "tags": []string{"project"}}), id + ".content": []byte(`{"pages":["first"],"fileType":"notebook","pageCount":1,"tags":[{"name":"project"}]}`), id + "/first.rm": page}
	for n, b := range files {
		p := filepath.Join(root, n)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(p, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	b, e := NewLocalDir(root)
	if e != nil {
		t.Fatal(e)
	}
	a, e := applicationFor(b, "local-dir")
	if e != nil {
		t.Fatal(e)
	}
	return a
}

func TestMCPNativeLocalWorkflow(t *testing.T) {
	t.Setenv("REMARKABLE_ROOT_PATH", "/work")
	a := localAppFixture(t)
	s := connectTestApp(t, a)
	tools, e := s.ListTools(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(tools.Tools) != 8 {
		t.Fatalf("got %d local tools", len(tools.Tools))
	}
	_, browse := callTestTool(t, s, "remarkable_browse", map[string]any{"tags": []string{"PROJECT"}})
	if len(browse["documents"].([]any)) != 1 {
		t.Fatalf("root/tag browse: %+v", browse)
	}
	_, read := callTestTool(t, s, "remarkable_read", map[string]any{"document": "/Notes"})
	if !strings.Contains(read["content"].(string), "Local native content café") || read["total_pages"] != float64(1) {
		t.Fatalf("read: %+v", read)
	}
	_, missing := callTestTool(t, s, "remarkable_read", map[string]any{"document": "/Other/Notes"})
	if missing["_error"] == nil {
		t.Fatal("outside-root document resolved")
	}
	img, _ := callTestTool(t, s, "remarkable_image", map[string]any{"document": "Notes"})
	if !strings.Contains(img.Content[0].(*mcp.TextContent).Text, "Page 1/1") {
		t.Fatalf("default page: %+v", img)
	}
	resource := img.Content[1].(*mcp.EmbeddedResource).Resource
	decoded, e := png.Decode(bytes.NewReader(resource.Blob))
	if e != nil {
		t.Fatal(e)
	}
	if decoded.Bounds().Dx() < 100 {
		t.Fatal("bad rendered dimensions")
	}
	_, svg := callTestTool(t, s, "remarkable_image", map[string]any{"document": "Notes", "compatibility": true, "output_format": "svg"})
	if !strings.Contains(svg["svg"].(string), "Local native content") {
		t.Fatal("SVG lacks typed content")
	}
	canvas, _ := callTestTool(t, s, "remarkable_canvas", map[string]any{"document": "Notes"})
	b, e := json.Marshal(canvas.StructuredContent)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte(`"writable":false`)) {
		t.Fatalf("local canvas writable: %s", b)
	}
	raw, e := s.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "remarkable:///Notes.txt"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(raw.Contents[0].Text, "Local native content") {
		t.Fatal("text resource missing content")
	}
	export, _ := callTestTool(t, s, "remarkable_export", map[string]any{"document": "Notes", "output_format": "markdown"})
	uri := export.Content[1].(*mcp.ResourceLink).URI
	exported, e := s.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(exported.Contents[0].Blob, []byte("Local native content")) {
		t.Fatal("export missing content")
	}
	for range 8 {
		callTestTool(t, s, "remarkable_export", map[string]any{"document": "Notes", "output_format": "markdown"})
	}
	if _, e = s.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri}); e == nil {
		t.Fatal("old export was not evicted")
	}
	prompts, e := s.ListPrompts(context.Background(), nil)
	if e != nil || len(prompts.Prompts) != 6 {
		t.Fatalf("prompts: %+v %v", prompts, e)
	}
	prompt, e := s.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "find_notes", Arguments: map[string]string{"topic": "native"}})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(prompt.Messages[0].Content.(*mcp.TextContent).Text, "native") {
		t.Fatal("prompt argument ignored")
	}
	if _, e = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "remarkable_read", Arguments: map[string]any{"document": 5}}); e == nil {
		t.Fatal("invalid document type accepted")
	}
	if _, e = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "remarkable_delete", Arguments: map[string]any{"document": "Notes"}}); e == nil {
		t.Fatal("local write exposed")
	}
}

func TestMCPTransportToolContracts(t *testing.T) {
	var all map[string]contract
	if e := json.Unmarshal(contractsJSON, &all); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"cloud", "ssh", "usb-web", "local-dir"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("REMARKABLE_READ_ONLY", "")
			a := localAppFixture(t)
			b := a.backend
			a.close()
			a, e := applicationFor(b, mode)
			if e != nil {
				t.Fatal(e)
			}
			s := connectTestApp(t, a)
			list, e := s.ListTools(context.Background(), nil)
			if e != nil {
				t.Fatal(e)
			}
			actual := map[string]*mcp.Tool{}
			for _, tool := range list.Tools {
				actual[tool.Name] = tool
			}
			for _, tool := range all[mode].Tools {
				got, ok := actual[tool.Name]
				if !ok {
					t.Fatalf("missing %s", tool.Name)
				}
				expected, _ := json.Marshal(tool.InputSchema)
				schema, _ := json.Marshal(got.InputSchema)
				if !bytes.Equal(expected, schema) {
					t.Fatalf("schema changed: %s", tool.Name)
				}
			}
			n := len(all[mode].Tools)
			if mode == "cloud" {
				n++
			}
			if len(actual) != n {
				t.Fatalf("tool count %d != %d", len(actual), n)
			}
		})
	}
}

func TestMCPNativeAuthorWorkflow(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	a, e := applicationFor(b, "ssh")
	if e != nil {
		t.Fatal(e)
	}
	s := connectTestApp(t, a)
	_, created := callTestTool(t, s, "remarkable_author", map[string]any{"method": "create_document", "name": "Native notebook", "text": "User supplied text", "defer_restart": true})
	if created["_error"] != nil {
		t.Fatalf("create failed: %+v", created)
	}
	id := created["uuid"].(string)
	before, e := b.Download(context.Background(), id)
	if e != nil {
		t.Fatal(e)
	}
	var content map[string]any
	if e = json.Unmarshal(before[id+".content"], &content); e != nil {
		t.Fatal(e)
	}
	cp := content["cPages"].(map[string]any)
	pageID := cp["pages"].([]any)[0].(map[string]any)["id"].(string)
	for range 2 {
		_, draw := callTestTool(t, s, "remarkable_author", map[string]any{"method": "draw", "document": id, "page": 1, "strokes": []map[string]any{{"points": [][]float64{{0.1, 0.3}, {0.8, 0.3}}, "tool": "fineliner"}}, "defer_restart": true})
		if draw["_error"] != nil {
			t.Fatalf("draw failed: %+v", draw)
		}
	}
	_, added := callTestTool(t, s, "remarkable_author", map[string]any{"method": "add_page", "document": id, "defer_restart": true})
	if added["_error"] != nil {
		t.Fatalf("add failed: %+v", added)
	}
	after, e := b.Download(context.Background(), id)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(after[pageID+".rm.bak"], before[pageID+".rm"]) {
		t.Fatal("original page backup was not preserved")
	}
	if !bytes.HasPrefix(after[pageID+".rm"], before[pageID+".rm"]) {
		t.Fatal("original ink/text was rewritten")
	}
	if e = json.Unmarshal(after[id+".content"], &content); e != nil {
		t.Fatal(e)
	}
	pages := content["cPages"].(map[string]any)["pages"].([]any)
	if len(pages) != 2 || pages[1].(map[string]any)["idx"].(map[string]any)["value"] != "bb" {
		t.Fatalf("new page index: %+v", pages)
	}
	_, read := callTestTool(t, s, "remarkable_read", map[string]any{"document": id})
	if !strings.Contains(read["content"].(string), "User supplied text") {
		t.Fatal("authoring lost typed text")
	}
	if _, e = os.Stat(filepath.Join(dir, "systemctl-calls")); !os.IsNotExist(e) {
		t.Fatal("deferred write restarted tablet")
	}
}

func TestMCPWriteResponseContract(t *testing.T) {
	t.Run("usb-web", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/documents/" {
				w.Write([]byte("[]"))
				return
			}
			if r.URL.Path != "/upload" || r.Method != "POST" {
				t.Errorf("unexpected USB request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(400)
			}
		}))
		defer server.Close()
		t.Setenv("REMARKABLE_USB_HOST", server.URL)
		backend, err := NewUSBWeb()
		if err != nil {
			t.Fatal(err)
		}
		a, err := applicationFor(backend, "usb-web")
		if err != nil {
			t.Fatal(err)
		}
		s := connectTestApp(t, a)
		file := filepath.Join(t.TempDir(), "Source.pdf")
		if err := os.WriteFile(file, nativeTestPDF(t), 0600); err != nil {
			t.Fatal(err)
		}
		_, uploaded := callTestTool(t, s, "remarkable_upload", map[string]any{"file_path": file, "document_name": "USB title", "parent_folder": "/Not an existing folder"})
		if uploaded["uploaded"] != true || uploaded["name"] != "USB title" || uploaded["format"] != "pdf" || uploaded["note"] == nil {
			t.Fatalf("USB upload: %+v", uploaded)
		}
		if _, ok := uploaded["uuid"]; ok {
			t.Fatalf("USB invented a UUID: %+v", uploaded)
		}
		if _, ok := uploaded["parent_folder"]; ok {
			t.Fatalf("USB claimed folder placement: %+v", uploaded)
		}
	})
	for _, mode := range []string{"cloud", "ssh"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("REMARKABLE_SKIP_CONFIRM", "1")
			var backend Backend
			if mode == "cloud" {
				_, backend = newSyncFixture(t)
			} else {
				backend, _ = fakeTransportSSH(t)
			}
			a, err := applicationFor(backend, mode)
			if err != nil {
				t.Fatal(err)
			}
			s := connectTestApp(t, a)
			_, folder := callTestTool(t, s, "remarkable_mkdir", map[string]any{"folder_name": "Destination", "defer_restart": true})
			if folder["created"] != true || folder["folder_name"] != "Destination" || folder["parent"] != "/" {
				t.Fatalf("mkdir: %+v", folder)
			}
			file := filepath.Join(t.TempDir(), "Source.pdf")
			if err := os.WriteFile(file, nativeTestPDF(t), 0600); err != nil {
				t.Fatal(err)
			}
			_, uploaded := callTestTool(t, s, "remarkable_upload", map[string]any{"file_path": file, "document_name": "Original", "defer_restart": true})
			if uploaded["uploaded"] != true || uploaded["format"] != "pdf" || uploaded["parent_folder"] != "/" {
				t.Fatalf("upload: %+v", uploaded)
			}
			id, ok := uploaded["uuid"].(string)
			if !ok || id == "" {
				t.Fatalf("missing upload UUID: %+v", uploaded)
			}
			_, moved := callTestTool(t, s, "remarkable_move", map[string]any{"document": id, "dest_folder": "/Destination", "defer_restart": true})
			if moved["moved"] != true || moved["name"] != "Original" || moved["from"] != "/Original" || moved["to"] != "/Destination" {
				t.Fatalf("move: %+v", moved)
			}
			_, renamed := callTestTool(t, s, "remarkable_rename", map[string]any{"document": id, "new_name": "Renamed", "defer_restart": true})
			if renamed["renamed"] != true || renamed["old_name"] != "Original" || renamed["new_name"] != "Renamed" {
				t.Fatalf("rename: %+v", renamed)
			}
			_, deleted := callTestTool(t, s, "remarkable_delete", map[string]any{"document": id, "defer_restart": true})
			if deleted["deleted"] != true || deleted["name"] != "Renamed" || deleted["path"] != "/Destination/Renamed" || deleted["type"] != "document" {
				t.Fatalf("delete: %+v", deleted)
			}
		})
	}
}

func TestMCPFolderMoveRejectsCycles(t *testing.T) {
	b, _ := fakeTransportSSH(t)
	ctx := WithDeferredRestart(context.Background(), true)
	parent, err := b.Mkdir(ctx, "Parent", "")
	if err != nil {
		t.Fatal(err)
	}
	child, err := b.Mkdir(ctx, "Child", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := applicationFor(b, "ssh")
	if err != nil {
		t.Fatal(err)
	}
	s := connectTestApp(t, a)
	for _, destination := range []string{"/Parent", "/Parent/Child"} {
		_, result := callTestTool(t, s, "remarkable_move", map[string]any{"document": "/Parent", "dest_folder": destination})
		if e, ok := result["_error"].(map[string]any); !ok || e["type"] != "invalid_move" {
			t.Fatalf("cycle accepted: %+v", result)
		}
	}
	docs, err := b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		if doc.ID == parent.ID && doc.Parent != "" {
			t.Fatal("rejected move changed parent")
		}
		if doc.ID == child.ID && doc.Parent != parent.ID {
			t.Fatal("rejected move changed child")
		}
	}
}

func TestMCPDeleteConfirmationAndExactPermanentTarget(t *testing.T) {
	for _, mode := range []string{"unsupported", "decline", "accept", "bypass"} {
		t.Run(mode, func(t *testing.T) {
			b, dir := fakeTransportSSH(t)
			t.Setenv("REMARKABLE_SKIP_CONFIRM", "")
			d, e := b.Upload(context.Background(), []byte("epub fixture"), "epub", "", "Delete target")
			if e != nil {
				t.Fatal(e)
			}
			neighbor := d.ID + "0.metadata"
			transportFixture(t, dir, neighbor, []byte(`{"visibleName":"Neighbor"}`))
			a, e := applicationFor(b, "ssh")
			if e != nil {
				t.Fatal(e)
			}
			var opts *mcp.ClientOptions
			if mode == "accept" || mode == "decline" {
				opts = &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					return &mcp.ElicitResult{Action: mode, Content: map[string]any{"confirm": true}}, nil
				}}
			}
			if mode == "bypass" {
				t.Setenv("REMARKABLE_SKIP_CONFIRM", "1")
			}
			s := connectTestApp(t, a, opts)
			_, result := callTestTool(t, s, "remarkable_delete", map[string]any{"document": d.ID, "permanent": true, "defer_restart": true})
			_, e = os.Stat(filepath.Join(dir, d.ID+".metadata"))
			if mode == "accept" || mode == "bypass" {
				if !os.IsNotExist(e) || result["deleted"] != true {
					t.Fatalf("confirmed deletion failed: %+v %v", result, e)
				}
			} else {
				if e != nil {
					t.Fatal("unconfirmed deletion changed files")
				}
			}
			if _, e = os.Stat(filepath.Join(dir, neighbor)); e != nil {
				t.Fatal("permanent deletion removed neighboring UUID")
			}
		})
	}
}
