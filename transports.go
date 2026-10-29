package main

// Native device transports. No operation invokes Python or writes a desktop
// sync directory. SSH writes are never replayed after an uncertain outcome.
import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrTransportReadOnly = errors.New("transport is read-only")
var ErrTransportUnsupported = errors.New("operation is unsupported by this transport")

// Embedding this implements all mutations for the unconditionally read-only
// local cache and the USB operations that firmware does not expose.
type unsupportedTransport struct{}

func (unsupportedTransport) Upload(context.Context, []byte, string, string, string) (Document, error) {
	return Document{}, ErrTransportReadOnly
}
func (unsupportedTransport) Overwrite(context.Context, string, []byte) error {
	return ErrTransportUnsupported
}
func (unsupportedTransport) Mkdir(context.Context, string, string) (Document, error) {
	return Document{}, ErrTransportUnsupported
}
func (unsupportedTransport) Move(context.Context, string, string) error {
	return ErrTransportUnsupported
}
func (unsupportedTransport) Rename(context.Context, string, string) error {
	return ErrTransportUnsupported
}
func (unsupportedTransport) Delete(context.Context, string) error { return ErrTransportUnsupported }
func (unsupportedTransport) Refresh()                             {}

func transportEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func expandTransportPath(p string) string {
	p = os.ExpandEnv(p)
	if p == "~" {
		h, _ := os.UserHomeDir()
		return h
	}
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, strings.TrimPrefix(p, "~/"))
	}
	return p
}
func validTransportID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00\r\n") && !strings.HasPrefix(id, "-")
}
func checkTransportID(id string) error {
	if !validTransportID(id) {
		return fmt.Errorf("invalid document ID %q", id)
	}
	return nil
}

type LocalDirBackend struct {
	unsupportedTransport
	Dir string
}

func NewLocalDir(dir string) (*LocalDirBackend, error) {
	if dir == "" {
		dir = os.Getenv("REMARKABLE_LOCAL_DIR")
	}
	switch strings.ToLower(dir) {
	case "", "1", "true", "yes", "auto":
		candidates := []string{"~/Library/Containers/com.remarkable.desktop/Data/Library/Application Support/remarkable/desktop", "~/Library/Application Support/remarkable/desktop", filepath.Join(os.Getenv("APPDATA"), "remarkable", "desktop"), "~/.local/share/remarkable/desktop"}
		for _, c := range candidates {
			if localDirUsable(expandTransportPath(c)) {
				return &LocalDirBackend{Dir: expandTransportPath(c)}, nil
			}
		}
		return nil, errors.New("cannot locate desktop sync directory; set REMARKABLE_LOCAL_DIR to a directory containing *.metadata")
	}
	dir = expandTransportPath(dir)
	if !localDirUsable(dir) {
		return nil, fmt.Errorf("local reMarkable directory is not usable: %s (requires *.metadata)", dir)
	}
	return &LocalDirBackend{Dir: dir}, nil
}
func localDirUsable(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".metadata") {
			return true
		}
	}
	return false
}

func parseTransportMetadata(id string, data, content []byte) (Document, bool) {
	var m map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&m) != nil || m == nil || m["deleted"] == true {
		return Document{}, false
	}
	d := Document{ID: id, Name: id, Type: "DocumentType"}
	if v, ok := m["visibleName"].(string); ok {
		d.Name = v
	}
	if v, ok := m["type"].(string); ok {
		d.Type = v
	}
	d.Parent, _ = m["parent"].(string)
	if v := m["lastModified"]; v != nil {
		ms, err := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		if err == nil {
			d.Modified = time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
		}
	}
	if tags, ok := m["tags"].([]any); ok {
		for _, t := range tags {
			switch v := t.(type) {
			case string:
				d.Tags = append(d.Tags, v)
			case map[string]any:
				if s, ok := v["name"].(string); ok {
					d.Tags = append(d.Tags, s)
				}
			}
		}
	}
	var c struct {
		FileType string `json:"fileType"`
		Tags     []any  `json:"tags"`
	}
	if json.Unmarshal(content, &c) == nil {
		d.FileType = c.FileType
		if len(d.Tags) == 0 {
			for _, tag := range c.Tags {
				switch tag := tag.(type) {
				case string:
					d.Tags = append(d.Tags, tag)
				case map[string]any:
					if name, ok := tag["name"].(string); ok {
						d.Tags = append(d.Tags, name)
					}
				}
			}
		}
	}
	return d, true
}
func (b *LocalDirBackend) List(ctx context.Context) ([]Document, error) {
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		return nil, err
	}
	docs := []Document{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(e.Name(), ".metadata") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".metadata")
		if !validTransportID(id) {
			continue
		}
		data, err := b.readSafe(e.Name())
		if err != nil {
			continue
		}
		c, _ := b.readSafe(id + ".content")
		if d, ok := parseTransportMetadata(id, data, c); ok {
			docs = append(docs, d)
		}
	}
	return docs, nil
}

// Do not follow cache symlinks outside the selected directory.
func (b *LocalDirBackend) readSafe(rel string) ([]byte, error) {
	root, err := os.OpenRoot(b.Dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
func (b *LocalDirBackend) Download(ctx context.Context, id string) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkTransportID(id); err != nil {
		return nil, err
	}
	if _, err := b.readSafe(id + ".metadata"); err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	err := filepath.WalkDir(filepath.Join(b.Dir, id), func(p string, e os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(b.Dir, p)
		data, err := b.readSafe(rel)
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(filepath.Join(b.Dir, id), p)
		files[filepath.ToSlash(name)] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, ext := range []string{"content", "pdf", "epub"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := b.readSafe(id + "." + ext)
		if err == nil {
			files[id+"."+ext] = data
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return files, nil
}

type USBWebBackend struct {
	unsupportedTransport
	Host   string
	Client *http.Client
	slots  chan struct{}
}

func NewUSBWeb() (*USBWebBackend, error) {
	timeout, err := strconv.Atoi(transportEnv("REMARKABLE_USB_TIMEOUT", "10"))
	if err != nil || timeout <= 0 {
		return nil, errors.New("REMARKABLE_USB_TIMEOUT must be a positive integer")
	}
	n, err := strconv.Atoi(transportEnv("REMARKABLE_USB_MAX_CONCURRENCY", "2"))
	if err != nil || n < 1 || n > 16 {
		return nil, errors.New("REMARKABLE_USB_MAX_CONCURRENCY must be between 1 and 16")
	}
	host := strings.TrimRight(transportEnv("REMARKABLE_USB_HOST", "http://10.11.99.1"), "/")
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("invalid REMARKABLE_USB_HOST")
	}
	return &USBWebBackend{Host: host, Client: &http.Client{Timeout: time.Duration(timeout) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, n)}, nil
}

type usbHTTPError struct {
	Status   int
	Endpoint string
}

func (e *usbHTTPError) Error() string {
	return fmt.Sprintf("USB web HTTP %d for %s", e.Status, e.Endpoint)
}
func (b *USBWebBackend) request(ctx context.Context, endpoint, method, contentType string, body []byte, timeout time.Duration) ([]byte, error) {
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	client := *b.Client
	if timeout > 0 {
		client.Timeout = timeout
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, b.Host+endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := client.Do(req)
		if err != nil {
			if method != "GET" {
				return nil, fmt.Errorf("USB upload outcome may be unknown; inspect tablet before retrying: %w", err)
			}
			return nil, err
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 408 && method == "GET" && attempt < 2 {
			select {
			case <-time.After(time.Duration(250*(1<<attempt)) * time.Millisecond):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &usbHTTPError{resp.StatusCode, endpoint}
		}
		if readErr != nil {
			return nil, readErr
		}
		return data, nil
	}
	return nil, errors.New("USB request exhausted")
}
func (b *USBWebBackend) List(ctx context.Context) ([]Document, error) {
	type folder struct{ id, endpoint string }
	queue := []folder{{"", "/documents/"}}
	seen := map[string]bool{}
	docs := []Document{}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if seen[f.endpoint] {
			continue
		}
		seen[f.endpoint] = true
		data, err := b.request(ctx, f.endpoint, "GET", "", nil, 0)
		if err != nil {
			return nil, err
		}
		var entries []struct {
			ID       string `json:"ID"`
			Name     string `json:"VissibleName"`
			Type     string `json:"Type"`
			Modified string `json:"ModifiedClient"`
			FileType string `json:"fileType"`
		}
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !validTransportID(e.ID) {
				return nil, errors.New("USB returned invalid document ID")
			}
			if e.Name == "" {
				e.Name = e.ID
			}
			if e.Type == "" {
				e.Type = "DocumentType"
			}
			if e.FileType == "" && e.Type != "CollectionType" {
				e.FileType = "notebook"
			}
			docs = append(docs, Document{ID: e.ID, Name: e.Name, Type: e.Type, Parent: f.id, Modified: e.Modified, FileType: e.FileType})
			if e.Type == "CollectionType" {
				queue = append(queue, folder{e.ID, "/documents/" + url.PathEscape(e.ID)})
			}
		}
	}
	return docs, nil
}
func unpackTransportZip(data []byte) (map[string][]byte, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, f := range z.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Name == "." || f.Name == ".." || path.Clean(f.Name) != f.Name || strings.HasPrefix(f.Name, "/") || strings.HasPrefix(f.Name, "../") || strings.Contains(f.Name, "\\") {
			return nil, errors.New("unsafe archive path")
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return nil, err
		}
		files[f.Name] = data
	}
	return files, nil
}
func (b *USBWebBackend) Download(ctx context.Context, id string) (map[string][]byte, error) {
	if err := checkTransportID(id); err != nil {
		return nil, err
	}
	data, err := b.request(ctx, "/download/"+url.PathEscape(id)+"/rmdoc", "GET", "", nil, 120*time.Second)
	var he *usbHTTPError
	if errors.As(err, &he) && he.Status == 404 {
		data, err = b.request(ctx, "/download/"+url.PathEscape(id)+"/pdf", "GET", "", nil, 120*time.Second)
		if err == nil {
			return map[string][]byte{id + ".pdf": data}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return unpackTransportZip(data)
}
func (b *USBWebBackend) Upload(ctx context.Context, data []byte, extension, parentID, name string) (Document, error) {
	if Enabled("REMARKABLE_READ_ONLY") {
		return Document{}, ErrTransportReadOnly
	}
	ext := strings.ToLower(strings.TrimPrefix(extension, "."))
	if ext != "pdf" && ext != "epub" && ext != "rmdoc" {
		return Document{}, ErrTransportUnsupported
	}
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if name == "." || name == "/" || name == "" {
		name = "Document"
	}
	filename := name
	if !strings.HasSuffix(strings.ToLower(filename), "."+ext) {
		filename += "." + ext
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	f, err := w.CreateFormFile("file", filename)
	if err != nil {
		return Document{}, err
	}
	if _, err = f.Write(data); err != nil {
		return Document{}, err
	}
	if err = w.Close(); err != nil {
		return Document{}, err
	}
	_, err = b.request(ctx, "/upload", "POST", w.FormDataContentType(), body.Bytes(), 120*time.Second)
	if err != nil {
		return Document{}, err
	}
	// The official endpoint does not return a UUID or support destination folders.
	return Document{Name: name, Type: "DocumentType", FileType: ext}, nil
}

const transportXochitl = "/home/root/.local/share/remarkable/xochitl"
const sshExecutionMarker = "__REMARKABLE_MCP_REMOTE_STARTED__"

// SSHExecutionUnknownError means a command may have executed. Never blindly retry.
type SSHExecutionUnknownError struct{ Err error }

func (e *SSHExecutionUnknownError) Error() string {
	return "SSH outcome unknown; inspect tablet before retrying: " + e.Err.Error()
}
func (e *SSHExecutionUnknownError) Unwrap() error { return e.Err }

type SSHPreExecutionError struct{ Err error }

func (e *SSHPreExecutionError) Error() string { return "SSH command did not start: " + e.Err.Error() }
func (e *SSHPreExecutionError) Unwrap() error { return e.Err }

type SSHRemoteCommandError struct{ Err error }

func (e *SSHRemoteCommandError) Error() string { return "SSH remote command failed: " + e.Err.Error() }
func (e *SSHRemoteCommandError) Unwrap() error { return e.Err }

type SSHBackend struct {
	Host, User, Key, Password string
	Port                      int
	mu                        sync.Mutex
	pending                   bool
	reliability               sshReliability
}

func NewSSH() (*SSHBackend, error) {
	p, err := strconv.Atoi(transportEnv("REMARKABLE_SSH_PORT", "22"))
	if err != nil || p < 1 || p > 65535 {
		return nil, errors.New("invalid REMARKABLE_SSH_PORT")
	}
	b := &SSHBackend{Host: transportEnv("REMARKABLE_SSH_HOST", "10.11.99.1"), User: transportEnv("REMARKABLE_SSH_USER", "root"), Port: p, Key: expandTransportPath(os.Getenv("REMARKABLE_SSH_KEY")), Password: os.Getenv("REMARKABLE_SSH_PASSWORD")}
	if strings.HasPrefix(b.Host, "-") || strings.ContainsAny(b.Host, "\x00\r\n @") || strings.ContainsAny(b.User, "\x00\r\n @") || strings.HasPrefix(b.User, "-") {
		return nil, errors.New("invalid SSH host or user")
	}
	if err := b.reliability.configure(); err != nil {
		return nil, err
	}
	return b, nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

var sshPreExecutionPattern = regexp.MustCompile(`(?im)^(ssh: connect to host .+: (Connection refused|Connection timed out|Operation timed out|No route to host|Network is unreachable|Host is down)|Connection timed out during banner exchange|kex_exchange_identification: Connection timed out)\r?$`)

func classifySSHFailure(code int, stdout, stderr []byte, err error) error {
	started := bytes.Contains(stderr, []byte(sshExecutionMarker))
	detail := strings.TrimSpace(strings.ReplaceAll(string(stderr), sshExecutionMarker, ""))
	if detail == "" {
		detail = err.Error()
	}
	cause := errors.New(detail)
	if code == 255 && !started && len(stdout) == 0 && sshPreExecutionPattern.Match(stderr) {
		return &SSHPreExecutionError{cause}
	}
	if started && code >= 0 && code != 255 {
		return &SSHRemoteCommandError{cause}
	}
	return &SSHExecutionUnknownError{cause}
}

// runOnce never replays a command; run retries only proven connection failures.
func (b *SSHBackend) runOnce(ctx context.Context, command string, input []byte, timeout time.Duration) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, &SSHPreExecutionError{err}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=accept-new", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-p", strconv.Itoa(b.Port)}
	if b.Password == "" {
		args = append(args, "-o", "BatchMode=yes")
	}
	if b.Key != "" {
		args = append(args, "-i", b.Key, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, b.User+"@"+b.Host, "printf '%s\\n' "+shellQuote(sshExecutionMarker)+" >&2; "+command)
	program := "ssh"
	if b.Password != "" {
		program = "sshpass"
		args = append([]string{"-e", "ssh"}, args...)
	}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = time.Second
	if b.Password != "" {
		cmd.Env = append(os.Environ(), "SSHPASS="+b.Password)
	}
	cmd.Stdin = bytes.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		failure := classifySSHFailure(code, out.Bytes(), stderr.Bytes(), err)
		if ctx.Err() != nil {
			return nil, &SSHExecutionUnknownError{errors.Join(failure, ctx.Err())}
		}
		return nil, failure
	}
	return out.Bytes(), nil
}
func (b *SSHBackend) Refresh() {} // Reads are uncached; tablet refresh is explicit.
func (b *SSHBackend) sshList(ctx context.Context) ([]Document, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.list(ctx)
}
func (b *SSHBackend) list(ctx context.Context) ([]Document, error) {
	// Read-only glob; NUL separators keep arbitrary JSON and filenames distinct.
	data, err := b.run(ctx, "cd "+shellQuote(transportXochitl)+" && for f in *.metadata *.content; do [ -f \"$f\" ] || continue; printf '%s\\0' \"$f\"; cat -- \"$f\" || exit; printf '\\0'; done", nil, 60*time.Second)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(data, []byte{0})
	files := map[string][]byte{}
	for i := 0; i+1 < len(parts); i += 2 {
		files[string(parts[i])] = parts[i+1]
	}
	keys := []string{}
	for k := range files {
		if strings.HasSuffix(k, ".metadata") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	docs := []Document{}
	for _, k := range keys {
		id := strings.TrimSuffix(k, ".metadata")
		if !validTransportID(id) {
			continue
		}
		if d, ok := parseTransportMetadata(id, files[k], files[id+".content"]); ok {
			docs = append(docs, d)
		}
	}
	return docs, nil
}
func (b *SSHBackend) sshDownload(ctx context.Context, id string) (map[string][]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.download(ctx, id)
}
func (b *SSHBackend) download(ctx context.Context, id string) (map[string][]byte, error) {
	if err := checkTransportID(id); err != nil {
		return nil, err
	}
	q := shellQuote(id)
	// tar follows neither directory nor file symlinks by default. Select exact
	// known sidecars; never use destructive/prefix globs for document IDs.
	// Keep the precondition separate so a missing metadata file cannot be hidden.
	command := "cd " + shellQuote(transportXochitl) + " && test -f " + shellQuote(id+".metadata") + " && { set --; for f in " + q + " " + shellQuote(id+".metadata") + " " + shellQuote(id+".content") + " " + shellQuote(id+".pdf") + " " + shellQuote(id+".epub") + "; do [ ! -e \"$f\" ] || set -- \"$@\" \"$f\"; done; if [ \"$#\" -gt 0 ]; then tar -cf - -- \"$@\"; fi; }"
	data, err := b.run(ctx, command, nil, 120*time.Second)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := h.Name
		if strings.HasPrefix(name, id+"/") {
			name = strings.TrimPrefix(name, id+"/")
		} else if name != id+".metadata" && name != id+".content" && name != id+".pdf" && name != id+".epub" {
			return nil, errors.New("unexpected SSH archive member")
		}
		if name == "." || name == ".." || path.Clean(name) != name || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			return nil, errors.New("unsafe SSH archive member")
		}
		d, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[name] = d
	}
	return files, nil
}
func transportMetadata(name, parent, kind string) map[string]any {
	return map[string]any{"visibleName": name, "parent": parent, "type": kind, "deleted": false, "pinned": false, "lastModified": strconv.FormatInt(time.Now().UnixMilli(), 10), "metadatamodified": true, "modified": true, "synced": false, "version": 0}
}
func (b *SSHBackend) writeFile(ctx context.Context, rel string, data []byte) error {
	// Stream bytes on stdin, not interpolated into a shell heredoc. Publish an
	// individual file atomically; multi-file operations can still partially persist.
	target := transportXochitl + "/" + rel
	tmp := target + ".mcp-" + NewID()
	return b.runChange(ctx, "umask 077; cat > "+shellQuote(tmp)+" && mv -f -- "+shellQuote(tmp)+" "+shellQuote(target), data, 120*time.Second)
}
func (b *SSHBackend) writeJSON(ctx context.Context, rel string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return b.writeFile(ctx, rel, data)
}
func (b *SSHBackend) validateParent(ctx context.Context, parent string) error {
	if parent == "" {
		return nil
	}
	if err := checkTransportID(parent); err != nil {
		return err
	}
	docs, err := b.list(ctx)
	if err != nil {
		return err
	}
	for _, d := range docs {
		if d.ID == parent && d.Type == "CollectionType" && d.Parent != "trash" {
			return nil
		}
	}
	return fmt.Errorf("parent folder %q not found", parent)
}

type deferredRestartContextKey struct{}

// WithDeferredRestart requests per-operation restart deferral without changing
// process-wide environment. False does not override REMARKABLE_DEFER_RESTART.
func WithDeferredRestart(ctx context.Context, deferRestart bool) context.Context {
	return context.WithValue(ctx, deferredRestartContextKey{}, deferRestart)
}

func (b *SSHBackend) finishWrite(ctx context.Context) error {
	return nil
}
func (b *SSHBackend) sshUpload(ctx context.Context, data []byte, extension, parent, name string) (Document, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if Enabled("REMARKABLE_READ_ONLY") {
		return Document{}, ErrTransportReadOnly
	}
	ext := strings.ToLower(strings.TrimPrefix(extension, "."))
	if ext != "pdf" && ext != "epub" {
		return Document{}, fmt.Errorf("SSH upload accepts PDF/EPUB only: %w", ErrTransportUnsupported)
	}
	if err := b.validateParent(ctx, parent); err != nil {
		return Document{}, err
	}
	id := NewID()
	m := transportMetadata(name, parent, "DocumentType")
	d := Document{ID: id, Name: name, Parent: parent, Type: "DocumentType", FileType: ext}
	if err := b.writeFile(ctx, id+"."+ext, data); err != nil {
		return d, err
	}
	if err := b.writeJSON(ctx, id+".content", map[string]any{"fileType": ext}); err != nil {
		return d, err
	}
	if err := b.runChange(ctx, "mkdir -p -- "+shellQuote(transportXochitl+"/"+id), nil, 30*time.Second); err != nil {
		return d, err
	}
	if err := b.writeJSON(ctx, id+".metadata", m); err != nil {
		return d, err
	}
	return d, b.finishWrite(ctx)
}
func (b *SSHBackend) sshMkdir(ctx context.Context, name, parent string) (Document, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if Enabled("REMARKABLE_READ_ONLY") {
		return Document{}, ErrTransportReadOnly
	}
	if err := b.validateParent(ctx, parent); err != nil {
		return Document{}, err
	}
	id := NewID()
	d := Document{ID: id, Name: name, Parent: parent, Type: "CollectionType"}
	if err := b.writeJSON(ctx, id+".metadata", transportMetadata(name, parent, "CollectionType")); err != nil {
		return d, err
	}
	return d, b.finishWrite(ctx)
}
func (b *SSHBackend) mutateMetadata(ctx context.Context, id string, change func(map[string]any) error) error {
	if Enabled("REMARKABLE_READ_ONLY") {
		return ErrTransportReadOnly
	}
	if err := checkTransportID(id); err != nil {
		return err
	}
	data, err := b.run(ctx, "cat -- "+shellQuote(transportXochitl+"/"+id+".metadata"), nil, 30*time.Second)
	if err != nil {
		return err
	}
	var m map[string]any
	if err = json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m == nil {
		return errors.New("invalid metadata")
	}
	if err = change(m); err != nil {
		return err
	}
	m["lastModified"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
	m["metadatamodified"] = true
	m["modified"] = true
	m["synced"] = false
	version, _ := m["version"].(float64)
	m["version"] = int(version) + 1
	if err = b.writeJSON(ctx, id+".metadata", m); err != nil {
		return err
	}
	return b.finishWrite(ctx)
}
func (b *SSHBackend) sshRename(ctx context.Context, id, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mutateMetadata(ctx, id, func(m map[string]any) error { m["visibleName"] = name; return nil })
}
func (b *SSHBackend) sshDelete(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mutateMetadata(ctx, id, func(m map[string]any) error { m["parent"] = "trash"; m["deleted"] = false; return nil })
}

func (b *SSHBackend) sshDeletePermanently(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if Enabled("REMARKABLE_READ_ONLY") {
		return ErrTransportReadOnly
	}
	if err := checkTransportID(id); err != nil {
		return err
	}
	docs, err := b.list(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, d := range docs {
		if d.Parent == id {
			return errors.New("cannot permanently delete a non-empty folder")
		}
		if d.ID == id {
			found = true
		}
	}
	if !found {
		return errors.New("document not found")
	}
	data, err := b.run(ctx, "find "+shellQuote(transportXochitl)+" -maxdepth 1 -type f -print0", nil, 30*time.Second)
	if err != nil {
		return err
	}
	args := []string{shellQuote(transportXochitl + "/" + id)}
	for _, p := range bytes.Split(data, []byte{0}) {
		name := path.Base(string(p))
		if strings.HasPrefix(name, id+".") {
			args = append(args, shellQuote(transportXochitl+"/"+name))
		}
	}
	if err = b.runChange(ctx, "rm -rf -- "+strings.Join(args, " "), nil, 120*time.Second); err != nil {
		return err
	}
	return b.finishWrite(ctx)
}
func (b *SSHBackend) sshMove(ctx context.Context, id, parent string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if Enabled("REMARKABLE_READ_ONLY") {
		return ErrTransportReadOnly
	}
	if err := b.validateParent(ctx, parent); err != nil {
		return err
	}
	docs, err := b.list(ctx)
	if err != nil {
		return err
	}
	parents := map[string]string{}
	for _, d := range docs {
		parents[d.ID] = d.Parent
	}
	seen := map[string]bool{}
	for p := parent; p != ""; p = parents[p] {
		if p == id || seen[p] {
			return errors.New("cannot move a folder into itself or a descendant (or cyclic hierarchy)")
		}
		seen[p] = true
	}
	return b.mutateMetadata(ctx, id, func(m map[string]any) error { m["parent"] = parent; return nil })
}
func (b *SSHBackend) sshOverwrite(ctx context.Context, id string, pdf []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if Enabled("REMARKABLE_READ_ONLY") {
		return ErrTransportReadOnly
	}
	if err := checkTransportID(id); err != nil {
		return err
	}
	metadata, err := b.run(ctx, "cat -- "+shellQuote(transportXochitl+"/"+id+".metadata"), nil, 30*time.Second)
	if err != nil {
		return err
	}
	var m map[string]any
	if json.Unmarshal(metadata, &m) != nil || m == nil || m["deleted"] == true || m["parent"] == "trash" || m["type"] == "CollectionType" {
		return errors.New("overwrite requires a live PDF document")
	}
	files, err := b.download(ctx, id)
	if err != nil {
		return err
	}
	old, ok := files[id+".pdf"]
	if !ok {
		return errors.New("document has no original PDF")
	}
	oldCount, err := PDFPageCount(old)
	if err != nil {
		return err
	}
	newCount, err := PDFPageCount(pdf)
	if err != nil {
		return err
	}
	if oldCount != newCount {
		return fmt.Errorf("PDF page count mismatch: existing %d, replacement %d", oldCount, newCount)
	}
	if err = b.writeFile(ctx, id+".pdf", pdf); err != nil {
		return err
	}
	return b.mutateMetadata(ctx, id, func(map[string]any) error { return nil })
}

// PutDocumentFiles supports native notebook creation and authoring. Keys are
// document-relative page paths (including pageID.rm) or exact ID.content /
// ID.metadata sidecars. It never deletes absent pages. Keys ending .rm.bak are
// published once, before replacement pages, and never replace an existing
// backup. Callers must supply the ORIGINAL page bytes for those keys.
// Metadata is published last; the entire multi-file update is not atomic.
func (b *SSHBackend) sshPutDocumentFiles(ctx context.Context, id string, files map[string][]byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if Enabled("REMARKABLE_READ_ONLY") {
		return ErrTransportReadOnly
	}
	if err := checkTransportID(id); err != nil {
		return err
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		if k == "" || k == "." || k == ".." || path.Clean(k) != k || strings.HasPrefix(k, "/") || strings.HasPrefix(k, "../") || strings.ContainsAny(k, "\\\x00\r\n") {
			return errors.New("unsafe document file path")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Publish first-edit backups before modifying any page. Atomic hard-link
	// creation refuses replacement even if another process races our existence
	// check. Failure stops the edit, rather than risking a missing original.
	for _, k := range keys {
		if !strings.HasSuffix(k, ".rm.bak") {
			continue
		}
		target := transportXochitl + "/" + id + "/" + k
		tmp := target + ".mcp-" + NewID()
		command := "mkdir -p -- " + shellQuote(path.Dir(target)) + " && { if [ -e " + shellQuote(target) + " ] || [ -L " + shellQuote(target) + " ]; then cat > /dev/null; else umask 077; cat > " + shellQuote(tmp) + " && { ln -- " + shellQuote(tmp) + " " + shellQuote(target) + "; result=$?; rm -f -- " + shellQuote(tmp) + "; exit \"$result\"; }; fi; }"
		err := b.runChange(ctx, command, files[k], 120*time.Second)
		if err != nil {
			return err
		}
	}
	for _, k := range keys {
		if k == id+".metadata" || strings.HasSuffix(k, ".rm.bak") {
			continue
		}
		rel := id + "/" + k
		if k == id+".content" || k == id+".pdf" || k == id+".epub" {
			rel = k
		}
		if err := b.runChange(ctx, "mkdir -p -- "+shellQuote(path.Dir(transportXochitl+"/"+rel)), nil, 30*time.Second); err != nil {
			return err
		}
		if err := b.writeFile(ctx, rel, files[k]); err != nil {
			return err
		}
	}
	if m, ok := files[id+".metadata"]; ok {
		if err := b.writeFile(ctx, id+".metadata", m); err != nil {
			return err
		}
	}
	return b.finishWrite(ctx)
}

// RefreshTablet is distinct from Refresh (cache invalidation in Backend).
// Restart once, require the same boot ID and two stable active-state probes.
func (b *SSHBackend) sshRefreshTablet(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if Enabled("REMARKABLE_READ_ONLY") {
		return ErrTransportReadOnly
	}
	return b.refreshTablet(ctx)
}
func (b *SSHBackend) refreshTablet(ctx context.Context) error {
	status, err := b.run(ctx, "systemctl is-active xochitl", nil, 10*time.Second)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(status)) != "active" {
		return errors.New("refusing restart: xochitl is not active")
	}
	boot, err := b.run(ctx, "cat /proc/sys/kernel/random/boot_id", nil, 10*time.Second)
	if err != nil {
		return err
	}
	boot = bytes.TrimSpace(boot)
	if len(boot) == 0 {
		return errors.New("empty tablet boot ID")
	}
	host, err := b.effectiveHost(ctx)
	if err != nil {
		return err
	}
	requireWeb := sshWebOpen(ctx, host)
	if _, err = b.run(ctx, "systemctl reset-failed xochitl", nil, 10*time.Second); err != nil {
		return err
	}
	if _, err = b.run(ctx, "systemctl restart xochitl", nil, 30*time.Second); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, b.reliability.readyTimeout)
	defer cancel()
	stable := 0
	for ctx.Err() == nil {
		result, e := b.run(ctx, "cat /proc/sys/kernel/random/boot_id", nil, 10*time.Second)
		if e != nil {
			stable = 0
			if err := sshWait(ctx, b.reliability.poll); err != nil {
				break
			}
			continue
		}
		if strings.TrimSpace(string(result)) != string(boot) {
			return errors.New("tablet rebooted during refresh; not restarting again")
		}
		state, statusErr := b.run(ctx, "systemctl is-active xochitl", nil, 10*time.Second)
		if statusErr == nil && strings.TrimSpace(string(state)) == "active" && (!requireWeb || sshWebOpen(ctx, host)) {
			stable++
			if stable == 2 {
				b.pending = false
				b.reliability.pendingSnapshot.Store(false)
				return nil
			}
		} else {
			stable = 0
		}
		delay := b.reliability.poll
		if stable == 1 {
			delay = b.reliability.settle
		}
		if err := sshWait(ctx, delay); err != nil {
			break
		}
	}
	return errors.New("xochitl readiness timed out; refresh remains pending")
}
func (b *SSHBackend) RefreshPending() bool { return b.reliability.pendingSnapshot.Load() }

func cloudTransportTokenAvailable() bool {
	if os.Getenv("REMARKABLE_TOKEN") != "" {
		return true
	}
	h, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(h, ".rmapi")} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// CLI flags are translated to these environment variables by the CLI owner.
// Like Python, a selected unreachable USB/SSH transport is retained if no
// configured cloud fallback exists; later operations may recover the device.
func SelectBackend() (Backend, string, error) {
	var b Backend
	var err error
	name := "cloud"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reachable := false
	switch {
	case Enabled("REMARKABLE_USE_LOCAL_DIR") || os.Getenv("REMARKABLE_LOCAL_DIR") != "":
		name = "local-dir"
		b, err = NewLocalDir("")
		reachable = err == nil
	case Enabled("REMARKABLE_USE_USB_WEB"):
		name = "usb-web"
		var u *USBWebBackend
		u, err = NewUSBWeb()
		if err == nil {
			b = u
			_, probeErr := u.request(ctx, "/documents/", "GET", "", nil, 0)
			reachable = probeErr == nil
		}
	case Enabled("REMARKABLE_USE_SSH"):
		name = "ssh"
		var s *SSHBackend
		s, err = NewSSH()
		if err == nil {
			b = s
			s.mu.Lock()
			_, probeErr := s.run(ctx, "echo ok", nil, 5*time.Second)
			s.mu.Unlock()
			reachable = probeErr == nil
		}
	default:
		b, err = NewCloud()
		return b, name, err
	}
	// Configuration parse errors in USB/SSH must not silently change transport.
	if err != nil && name != "local-dir" {
		return nil, name, err
	}
	if !reachable && !Enabled("REMARKABLE_DISABLE_CLOUD_FALLBACK") && cloudTransportTokenAvailable() {
		c, e := NewCloud()
		return c, "cloud", e
	}
	return b, name, err
}
