package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPythonMCPCompatibility(t *testing.T) {
	python := os.Getenv("REMARKABLE_PYTHON_ORACLE")
	if python == "" {
		t.Skip("set REMARKABLE_PYTHON_ORACLE to the reference Python interpreter")
	}
	t.Setenv("REMARKABLE_ROOT_PATH", "/work")
	a := localAppFixture(t)
	root := a.backend.(*LocalDirBackend).Dir
	id := "00000000-0000-4000-8000-000000000003"
	files := map[string][]byte{id + ".metadata": jsonBytes(map[string]any{"visibleName": "Paper", "type": "DocumentType", "parent": "00000000-0000-4000-8000-000000000002", "lastModified": "1700000010123"}), id + ".content": []byte(`{"pages":["first","second"],"fileType":"pdf","pageCount":2}`), id + ".pdf": nativeTestPDF(t)}
	files["00000000-0000-4000-8000-000000000001/extra.txt"] = []byte("Second typed excerpt\nwith ``` embedded fence\n")
	longID := "00000000-0000-4000-8000-000000000004"
	longPage, e := NewNotebookPage(strings.Repeat("é", 8100) + " boundary end (?P<word>)")
	if e != nil {
		t.Fatal(e)
	}
	files[longID+".metadata"] = jsonBytes(map[string]any{"visibleName": "Unicode", "type": "DocumentType", "parent": "00000000-0000-4000-8000-000000000002"})
	files[longID+".content"] = []byte(`{"pages":["long"],"fileType":"notebook","pageCount":1}`)
	if e = os.Mkdir(filepath.Join(root, longID), 0700); e != nil {
		t.Fatal(e)
	}
	files[longID+"/long.rm"] = longPage
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	native := connectTestApp(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.Command(python, "-c", "from remarkable_mcp.cli import main; main()")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "REMARKABLE_") {
			command.Env = append(command.Env, v)
		}
	}
	command.Env = append(command.Env, "REMARKABLE_USE_LOCAL_DIR=1", "REMARKABLE_LOCAL_DIR="+root, "REMARKABLE_ROOT_PATH=/work")
	client := mcp.NewClient(&mcp.Implementation{Name: "compatibility", Version: "1"}, nil)
	reference, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"remarkable_status", map[string]any{}},
		{"remarkable_browse", map[string]any{}},
		{"remarkable_browse", map[string]any{"query": "o"}},
		{"remarkable_browse", map[string]any{"tags": []string{"PROJECT"}}},
		{"remarkable_browse", map[string]any{"path": "/Notes"}},
		{"remarkable_recent", map[string]any{"include_preview": true}},
		{"remarkable_read", map[string]any{"document": "Notes"}},
		{"remarkable_read", map[string]any{"document": "Paper", "content_type": "raw"}},
		{"remarkable_read", map[string]any{"document": "Paper", "content_type": "annotations"}},
		{"remarkable_read", map[string]any{"document": "Notes", "grep": `(?<=native )content`}},
		{"remarkable_read", map[string]any{"document": "Notes", "grep": `(?P<word>native)\s+(?P=word)`}},
		{"remarkable_read", map[string]any{"document": "Notes", "page": 0}},
		{"remarkable_read", map[string]any{"document": "Unicode", "page": 2}},
		{"remarkable_read", map[string]any{"document": "Unicode", "grep": `(?<=é )boundary(?= end)`}},
		{"remarkable_read", map[string]any{"document": "Unicode", "grep": `\(\?P<word>\)\Z`}},
		{"remarkable_search", map[string]any{"query": "o", "tags": []string{"project"}}},
		{"remarkable_read", map[string]any{"document": "Notes", "page": 2}},
		{"remarkable_image", map[string]any{"document": "Paper", "compatibility": true}},
		{"remarkable_export", map[string]any{"document": "Paper", "include_ocr": true}},
	} {
		t.Run(test.name+"/"+string(jsonBytes(test.args)), func(t *testing.T) {
			_, got := callTestTool(t, native, test.name, test.args)
			_, want := callTestTool(t, reference, test.name, test.args)
			for _, data := range []map[string]any{got, want} {
				delete(data, "_hint")
				if e, ok := data["_error"].(map[string]any); ok {
					delete(e, "message")
					delete(e, "suggestion")
					delete(e, "did_you_mean")
				}
				if test.name == "remarkable_image" {
					delete(data, "data_uri")
					delete(data, "image_base64")
				}
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("native:\n%s\nPython:\n%s", g, w)
			}
		})
	}
	for _, document := range []string{"Notes", "Paper"} {
		t.Run("export-bytes/"+document, func(t *testing.T) {
			read := func(session *mcp.ClientSession) []byte {
				result, _ := callTestTool(t, session, "remarkable_export", map[string]any{"document": document, "output_format": "markdown"})
				link := result.Content[1].(*mcp.ResourceLink)
				resource, e := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: link.URI})
				if e != nil {
					t.Fatal(e)
				}
				if resource.Contents[0].Text != "" {
					return []byte(resource.Contents[0].Text)
				}
				return resource.Contents[0].Blob
			}
			got, want := read(native), read(reference)
			if !bytes.Equal(got, want) {
				t.Errorf("native:\n%s\nPython:\n%s", got, want)
			}
		})
	}
}
