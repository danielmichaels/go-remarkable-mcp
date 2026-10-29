package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (a *application) write(ctx context.Context, name string, v map[string]any, docs []Document, paths map[string]string) (*mcp.CallToolResult, error) {
	ctx = WithDeferredRestart(ctx, boolean(v, "defer_restart"))
	result := map[string]any{"transport": a.transport}
	var err error
	switch name {
	case "remarkable_upload", "remarkable_markdown_to_pdf":
		var parent string
		var e error
		if a.transport != "usb-web" {
			parent, e = a.folder(docs, paths, str(v, "parent_folder"))
			if e != nil {
				return nil, e
			}
		}
		var data []byte
		ext, title := "pdf", str(v, "document_name")
		if name == "remarkable_upload" {
			file := str(v, "file_path")
			ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(file)), ".")
			if ext != "pdf" && ext != "epub" {
				return nil, errors.New("upload accepts PDF or EPUB")
			}
			data, e = os.ReadFile(file)
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
			}
		} else {
			data, e = MarkdownPDF(str(v, "markdown"), title)
		}
		if e != nil {
			return nil, e
		}
		d, e := a.backend.Upload(ctx, data, ext, parent, title)
		err = e
		result["uploaded"] = e == nil
		result["uuid"] = d.ID
		result["name"] = d.Name
		result["format"] = ext
		result["parent_folder"] = str(v, "parent_folder")
		if a.transport == "ssh" {
			result["remote_path"] = transportXochitl + "/" + d.ID + "." + ext
		}
		if a.transport == "usb-web" {
			delete(result, "uuid")
			delete(result, "parent_folder")
			if str(v, "parent_folder") != "/" {
				result["note"] = "USB web ignores parent_folder. The tablet web service chooses the destination folder. Use SSH or cloud mode for explicit folder placement."
			}
		}
	case "remarkable_mkdir":
		parent, e := a.folder(docs, paths, str(v, "parent"))
		if e != nil {
			return nil, e
		}
		if strings.TrimSpace(str(v, "folder_name")) == "" {
			return nil, errors.New("folder name is required")
		}
		d, e := a.backend.Mkdir(ctx, str(v, "folder_name"), parent)
		err = e
		result["created"] = e == nil
		result["uuid"] = d.ID
		result["folder_name"] = d.Name
		result["parent"] = str(v, "parent")
	case "remarkable_move", "remarkable_rename", "remarkable_delete":
		d, e := resolve(docs, paths, str(v, "document"))
		if e != nil {
			return nil, e
		}
		if paths[d.ID] == "/" {
			return nil, errors.New("cannot modify the configured root folder")
		}
		switch name {
		case "remarkable_move":
			parent, e := a.folder(docs, paths, str(v, "dest_folder"))
			if e != nil {
				return nil, e
			}
			if d.Type == "CollectionType" {
				byID := map[string]Document{}
				for _, item := range docs {
					byID[item.ID] = item
				}
				for id := parent; id != ""; id = byID[id].Parent {
					if id == d.ID {
						return failure("invalid_move", "Cannot move a folder into itself or one of its subfolders", "Choose a destination that is not inside the folder being moved."), nil
					}
					if _, ok := byID[id]; !ok {
						break
					}
				}
			}
			err = a.backend.Move(ctx, d.ID, parent)
			result["moved"] = err == nil
			result["name"] = d.Name
			result["from"] = paths[d.ID]
			result["to"] = str(v, "dest_folder")
		case "remarkable_rename":
			if strings.TrimSpace(str(v, "new_name")) == "" {
				return nil, errors.New("new_name is required")
			}
			err = a.backend.Rename(ctx, d.ID, str(v, "new_name"))
			result["renamed"] = err == nil
			result["old_name"] = d.Name
			result["new_name"] = str(v, "new_name")
		case "remarkable_delete":
			if boolean(v, "permanent") {
				b, ok := a.backend.(*SSHBackend)
				if !ok {
					return nil, errors.New("permanent deletion requires SSH")
				}
				err = b.DeletePermanently(ctx, d.ID)
			} else {
				err = a.backend.Delete(ctx, d.ID)
			}
			result["deleted"] = err == nil
			result["name"] = d.Name
			result["path"] = paths[d.ID]
			result["type"] = "document"
			if d.Type == "CollectionType" {
				result["type"] = "folder"
			}
			if a.transport == "ssh" {
				result["permanent"] = boolean(v, "permanent")
			}
		}
	case "remarkable_overwrite_pdf":
		id := str(v, "document_id")
		d, e := resolve(docs, paths, id)
		if e != nil {
			return nil, e
		}
		if d.ID != id {
			return nil, errors.New("overwrite requires the exact document UUID")
		}
		data, e := os.ReadFile(str(v, "file_path"))
		if e != nil {
			return nil, e
		}
		err = a.backend.Overwrite(ctx, id, data)
		result["overwritten"] = err == nil
		result["uuid"] = id
	case "remarkable_author":
		return a.author(ctx, v, docs, paths)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return nil, err
	}
	a.backend.Refresh()
	if b, ok := a.backend.(*SSHBackend); ok {
		result["refresh_pending"] = b.RefreshPending()
		if name != "remarkable_upload" && name != "remarkable_markdown_to_pdf" && name != "remarkable_overwrite_pdf" {
			delete(result, "transport")
		}
	}
	return response(result, "Write completed."), nil
}

func (a *application) author(ctx context.Context, v map[string]any, docs []Document, paths map[string]string) (*mcp.CallToolResult, error) {
	b, ok := a.backend.(*SSHBackend)
	if !ok {
		return nil, errors.New("native authoring requires SSH")
	}
	method := str(v, "method")
	var id string
	var files map[string][]byte
	pageNumber := integer(v, "page")
	changes := map[string][]byte{}
	if method == "create_document" {
		name := strings.TrimSpace(str(v, "name"))
		if name == "" {
			return nil, errors.New("name is required")
		}
		parent, e := a.folder(docs, paths, str(v, "folder"))
		if e != nil {
			return nil, e
		}
		id = NewID()
		pageID := NewID()
		page, e := NewNotebookPage(str(v, "text"))
		if e != nil {
			return nil, e
		}
		changes[pageID+".rm"] = page
		content, e := NotebookContent(pageID, page)
		if e != nil {
			return nil, e
		}
		changes[id+".content"] = jsonBytes(content)
		changes[id+".metadata"] = jsonBytes(map[string]any{"visibleName": name, "parent": parent, "type": "DocumentType", "version": 1, "lastModified": fmt.Sprint(time.Now().UnixMilli()), "metadatamodified": true})
		pageNumber = 1
	} else {
		d, e := resolve(docs, paths, str(v, "document"))
		if e != nil {
			return nil, e
		}
		id = d.ID
		files, e = b.Download(ctx, id)
		if e != nil {
			return nil, e
		}
		archive, e := readRenderArchive(files)
		if e != nil {
			return nil, e
		}
		switch method {
		case "draw":
			if pageNumber < 1 || pageNumber > len(archive.order) {
				return nil, errors.New("page_out_of_range")
			}
			pageID := archive.order[pageNumber-1]
			if pageID == "" {
				return nil, errors.New("page has no stable ID; cannot safely attach ink")
			}
			original := archive.rm[pageID]
			if len(original) == 0 {
				original, e = NewNotebookPage("")
				if e != nil {
					return nil, e
				}
			}
			var strokes []map[string]any
			data, e := json.Marshal(v["strokes"])
			if e != nil {
				return nil, e
			}
			if e = json.Unmarshal(data, &strokes); e != nil {
				return nil, e
			}
			if len(strokes) == 0 {
				return nil, errors.New("strokes are required")
			}
			updated, e := AuthorPage(original, strokes)
			if e != nil {
				return nil, e
			}
			if raw := archive.rm[pageID]; len(raw) > 0 {
				changes[pageID+".rm.bak"] = raw
			}
			changes[pageID+".rm"] = updated
		case "add_page":
			if DocumentFileType(files) != "notebook" {
				return nil, errors.New("add_page accepts native notebooks only")
			}
			var content map[string]any
			if e = json.Unmarshal(files[id+".content"], &content); e != nil {
				return nil, e
			}
			pageID := NewID()
			author := ""
			if cp, ok := content["cPages"].(map[string]any); ok {
				pages, _ := cp["pages"].([]any)
				last := ""
				for _, p := range pages {
					if p, ok := p.(map[string]any); ok {
						if idx, ok := p["idx"].(map[string]any); ok {
							if value, ok := idx["value"].(string); ok && value > last {
								last = value
							}
						}
					}
				}
				idx := "ba"
				if last != "" {
					r := []rune(last)
					if r[len(r)-1] < 'z' {
						r[len(r)-1]++
						idx = string(r)
					} else {
						idx = last + "a"
					}
				}
				pages = append(pages, map[string]any{"id": pageID, "idx": map[string]any{"timestamp": "1:2", "value": idx}, "template": map[string]any{"timestamp": "1:2", "value": "Blank"}})
				cp["pages"] = pages
				if uuids, ok := cp["uuids"].([]any); ok && len(uuids) > 0 {
					if first, ok := uuids[0].(map[string]any); ok {
						author, _ = first["first"].(string)
					}
				}
			} else {
				return nil, errors.New("add_page requires a native cPages notebook")
			}
			blank, e := NewNotebookPageWithAuthor("", author)
			if e != nil {
				return nil, e
			}
			changes[pageID+".rm"] = blank
			pageNumber = len(archive.order) + 1
			content["pageCount"] = pageNumber
			changes[id+".content"] = jsonBytes(content)
		default:
			return nil, errors.New("method must be draw, add_page, or create_document")
		}
		var metadata map[string]any
		if e = json.Unmarshal(files[id+".metadata"], &metadata); e != nil {
			return nil, e
		}
		nativeBump(metadata)
		changes[id+".metadata"] = jsonBytes(metadata)
	}
	if err := b.PutDocumentFiles(ctx, id, changes); err != nil {
		return nil, err
	}
	return response(map[string]any{"method": method, "uuid": id, "page": pageNumber, "paper_size": []int{1404, 1872}, "refresh_pending": b.RefreshPending()}, "Native notebook write completed."), nil
}

func registerDevice(code string) error {
	if strings.TrimSpace(code) == "" {
		return errors.New("device code is required")
	}
	body := jsonBytes(map[string]string{"code": code, "deviceDesc": "desktop-windows", "deviceID": NewID()})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://webapp-prod.cloud.remarkable.engineering/token/json/2/device/new", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registration HTTP %d", resp.StatusCode)
	}
	token, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(token)) == 0 {
		return errors.New("registration returned an empty token")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	target := filepath.Join(home, ".rmapi")
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("refusing to replace credentials at %s: %w", target, err)
	}
	_, writeErr := file.Write(jsonBytes(map[string]string{"devicetoken": strings.TrimSpace(string(token))}))
	return errors.Join(writeErr, file.Close())
}
