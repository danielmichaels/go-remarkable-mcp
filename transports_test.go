package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func transportFixture(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func transportZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for k, v := range files {
		f, e := w.Create(k)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(v); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

func TestNativeLocalDirectory(t *testing.T) {
	dir := t.TempDir()
	transportFixture(t, dir, "a.metadata", []byte(`{"visibleName":"Ink","parent":"folder","lastModified":"1700000000000","tags":[{"name":"work"},"read"]}`))
	transportFixture(t, dir, "a.content", []byte(`{"fileType":"pdf","pages":["page"]}`))
	transportFixture(t, dir, "a/sub/page.rm", []byte("ink"))
	transportFixture(t, dir, "a.pdf", []byte("source"))
	transportFixture(t, dir, "b.metadata", []byte(`{"deleted":true}`))
	transportFixture(t, dir, "c.metadata", []byte("not JSON"))
	transportFixture(t, dir, "folder.metadata", []byte(`{"visibleName":"Folder","type":"CollectionType"}`))
	b, err := NewLocalDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := b.List(context.Background())
	if err != nil || len(docs) != 2 {
		t.Fatalf("list=%+v err=%v", docs, err)
	}
	if docs[0].FileType != "pdf" || docs[0].Modified != "2023-11-14T22:13:20Z" || !reflect.DeepEqual(docs[0].Tags, []string{"work", "read"}) {
		t.Fatalf("metadata=%+v", docs[0])
	}
	files, err := b.Download(context.Background(), "a")
	if err != nil || string(files["sub/page.rm"]) != "ink" || string(files["a.pdf"]) != "source" || len(files) != 3 {
		t.Fatalf("download=%v err=%v", files, err)
	}
	if _, err = b.Download(context.Background(), "../a"); err == nil {
		t.Fatal("accepted traversal")
	}
	t.Setenv("REMARKABLE_READ_ONLY", "0")
	if _, err = b.Upload(context.Background(), nil, "pdf", "", "x"); !errors.Is(err, ErrTransportReadOnly) {
		t.Fatal(err)
	}
	if err = b.Delete(context.Background(), "a"); err == nil {
		t.Fatal("local delete accepted")
	}
	transportFixture(t, dir, "a.metadata", []byte(`{"visibleName":"Changed"}`))
	b.Refresh()
	docs, err = b.List(context.Background())
	if err != nil || docs[0].Name != "Changed" {
		t.Fatalf("fresh list %+v %v", docs, err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err = os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "a", "escape.rm")); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Download(context.Background(), "a"); err == nil {
		t.Fatal("followed escaping symlink")
	}
}

func TestNativeUSBTraversalDownloadAndUpload(t *testing.T) {
	archive := transportZip(t, map[string][]byte{"doc.content": []byte(`{"fileType":"pdf"}`), "doc/page.rm": []byte("ink"), "doc.pdf": []byte("source")})
	var uploads atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/documents/":
			io.WriteString(w, `[{"ID":"folder","VissibleName":"Folder","Type":"CollectionType"}]`)
		case "/documents/folder":
			io.WriteString(w, `[{"ID":"doc","VissibleName":"Notes","Type":"DocumentType","fileType":"pdf"},{"ID":"folder","VissibleName":"Cycle","Type":"CollectionType"}]`)
		case "/download/doc/rmdoc":
			w.Write(archive)
		case "/download/legacy/rmdoc":
			http.NotFound(w, r)
		case "/download/legacy/pdf":
			io.WriteString(w, "%PDF-legacy")
		case "/upload":
			uploads.Add(1)
			if r.Method != "POST" {
				t.Error("not POST")
			}
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Error(err)
				http.Error(w, "multipart", 400)
				return
			}
			defer r.MultipartForm.RemoveAll()
			f, h, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			data, _ := io.ReadAll(f)
			if h.Filename != "Report.pdf" || string(data) != "bytes" {
				t.Errorf("filename=%s data=%s", h.Filename, data)
			}
			w.WriteHeader(201)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	t.Setenv("REMARKABLE_USB_HOST", s.URL)
	t.Setenv("REMARKABLE_READ_ONLY", "")
	b, err := NewUSBWeb()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	docs, err := b.List(ctx)
	if err != nil || len(docs) != 3 || docs[1].Parent != "folder" {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	files, err := b.Download(ctx, "doc")
	if err != nil || string(files["doc.pdf"]) != "source" {
		t.Fatalf("files=%v err=%v", files, err)
	}
	files, err = b.Download(ctx, "legacy")
	if err != nil || string(files["legacy.pdf"]) != "%PDF-legacy" {
		t.Fatalf("fallback=%v err=%v", files, err)
	}
	d, err := b.Upload(ctx, []byte("bytes"), "pdf", "ignored-folder", "../Report")
	if err != nil || d.ID != "" || d.Parent != "" {
		t.Fatalf("upload=%+v err=%v", d, err)
	}
	t.Setenv("REMARKABLE_READ_ONLY", "1")
	if _, err = b.Upload(ctx, nil, "pdf", "", "x"); !errors.Is(err, ErrTransportReadOnly) {
		t.Fatal(err)
	}
	if uploads.Load() != 1 {
		t.Fatal("read-only upload sent")
	}
	if err = b.Delete(ctx, "doc"); !errors.Is(err, ErrTransportUnsupported) {
		t.Fatal(err)
	}
}

func TestNativeUSBRetryOnlyGET408(t *testing.T) {
	var gets, posts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if gets.Add(1) < 3 {
				w.WriteHeader(408)
				return
			}
			io.WriteString(w, "[]")
			return
		}
		posts.Add(1)
		w.WriteHeader(408)
	}))
	defer s.Close()
	t.Setenv("REMARKABLE_USB_HOST", s.URL)
	t.Setenv("REMARKABLE_READ_ONLY", "")
	b, err := NewUSBWeb()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 3 {
		t.Fatal(gets.Load())
	}
	if _, err = b.Upload(context.Background(), nil, "pdf", "", "x"); err == nil {
		t.Fatal("408 upload succeeded")
	}
	if posts.Load() != 1 {
		t.Fatal("POST was replayed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = b.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestNativeUSBRejectsUnsafeArchive(t *testing.T) {
	for _, name := range []string{".", "..", "../bad", "/absolute", "a/../../bad", "a\\bad"} {
		if _, err := unpackTransportZip(transportZip(t, map[string][]byte{name: []byte("x")})); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

// Fake SSH executes only against a temporary xochitl fixture. The exact remote
// path is replaced before the shell runs; no real tablet/network is contacted.
func fakeTransportSSH(t *testing.T) (*SSHBackend, string) {
	t.Helper()
	bin := t.TempDir()
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = -G ]; then printf 'hostname 127.0.0.1\n'; exit 0; fi
for command do :; done
printf 'call\n' >> "$FAKE_SSH_LOG"
if [ -n "$FAKE_SSH_REFUSE_FIRST" ] && [ "$(wc -l < "$FAKE_SSH_LOG")" -le "$FAKE_SSH_REFUSE_FIRST" ]; then printf 'ssh: connect to host fake port 22: Connection refused\n' >&2; exit 255; fi
if [ -f "$FAKE_XOCHITL/block" ]; then
 printf '__REMARKABLE_MCP_REMOTE_STARTED__\n' >&2
 touch "$FAKE_XOCHITL/started"
 while [ -f "$FAKE_XOCHITL/block" ]; do sleep 0.01; done
fi
if [ "$FAKE_SSH_FAILURE" = unknown ]; then printf '__REMARKABLE_MCP_REMOTE_STARTED__\n' >&2; exit 255; fi
if [ "$FAKE_SSH_FAILURE" = refused ]; then printf 'ssh: connect to host fake port 22: Connection refused\n' >&2; exit 255; fi
command=$(printf '%s' "$command" | sed "s@/home/root/.local/share/remarkable/xochitl@$FAKE_XOCHITL@g; s@/proc/sys/kernel/random/boot_id@$FAKE_XOCHITL/boot-id@g")
printf '%s\n' "$command" >> "$FAKE_XOCHITL/commands"
exec sh -c "$command"
`
	p := filepath.Join(bin, "ssh")
	if err := os.WriteFile(p, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	systemctl := `#!/bin/sh
printf '%s\n' "$1" >> "$FAKE_XOCHITL/systemctl-calls"
case "$1" in
is-active) printf '%s\n' "${FAKE_XOCHITL_STATUS:-active}" ;;
restart)
 touch "$FAKE_XOCHITL/restarting"
 while [ -f "$FAKE_XOCHITL/block-restart" ]; do sleep 0.01; done
 if [ "$FAKE_REBOOT" = 1 ]; then printf 'new-boot\n' > "$FAKE_XOCHITL/boot-id"; fi ;;
reset-failed) : ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(systemctl), 0700); err != nil {
		t.Fatal(err)
	}
	transportFixture(t, dir, "boot-id", []byte("original-boot\n"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_XOCHITL", dir)
	t.Setenv("FAKE_SSH_LOG", filepath.Join(dir, "calls"))
	t.Setenv("FAKE_SSH_FAILURE", "")
	t.Setenv("FAKE_REBOOT", "")
	t.Setenv("FAKE_XOCHITL_STATUS", "active")
	t.Setenv("REMARKABLE_DEFER_RESTART", "1")
	t.Setenv("REMARKABLE_READ_ONLY", "")
	t.Setenv("REMARKABLE_SSH_PASSWORD", "")
	t.Setenv("REMARKABLE_SSH_KEY", "")
	t.Setenv("REMARKABLE_SSH_HOST", "fake")
	t.Setenv("REMARKABLE_SSH_USER", "root")
	t.Setenv("REMARKABLE_SSH_PORT", "22")
	t.Setenv("REMARKABLE_RESTART_SETTLE", "0.01")
	t.Setenv("REMARKABLE_RESTART_POLL_INTERVAL", "0.01")
	t.Setenv("REMARKABLE_SSH_BACKOFF_INITIAL", "0.01")
	t.Setenv("REMARKABLE_SSH_BACKOFF_MAX", "0.01")
	t.Setenv("FAKE_SSH_REFUSE_FIRST", "")
	b, err := NewSSH()
	if err != nil {
		t.Fatal(err)
	}
	return b, dir
}

func TestNativeSSHFixtureMutations(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	ctx := context.Background()
	folder, err := b.Mkdir(ctx, "Folder", "|")
	if err == nil {
		t.Fatalf("invalid parent accepted: %+v", folder)
	}
	folder, err = b.Mkdir(ctx, "Folder", "")
	if err != nil {
		t.Fatal(err)
	}
	name := "quote ' ; $(touch SHOULD_NOT_EXIST)"
	d, err := b.Upload(ctx, []byte("epub bytes"), "epub", folder.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := b.List(ctx)
	if err != nil || len(docs) != 2 {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	if err = b.Rename(ctx, d.ID, "Renamed ' ; $()"); err != nil {
		t.Fatal(err)
	}
	if err = b.Move(ctx, folder.ID, folder.ID); err == nil {
		t.Fatal("self-move accepted")
	}
	if err = b.PutDocumentFiles(ctx, d.ID, map[string][]byte{"sub/page.rm": []byte("ink")}); err != nil {
		t.Fatal(err)
	}
	files, err := b.Download(ctx, d.ID)
	if err != nil || string(files[d.ID+".epub"]) != "epub bytes" || string(files["sub/page.rm"]) != "ink" {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if !b.RefreshPending() {
		t.Fatal("pending refresh lost")
	}
	if err = b.Delete(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, d.ID+".metadata"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["parent"] != "trash" || m["deleted"] != false {
		t.Fatalf("delete=%s", data)
	}
	if _, err = os.Stat(filepath.Join(dir, d.ID+".epub")); err != nil {
		t.Fatal("trash deleted source", err)
	}
	for _, key := range []string{".", "..", "../escape", "/absolute"} {
		if err = b.PutDocumentFiles(ctx, d.ID, map[string][]byte{key: nil}); err == nil {
			t.Fatalf("unsafe author path accepted: %q", key)
		}
	}
	t.Setenv("REMARKABLE_READ_ONLY", "true")
	if err = b.Rename(ctx, d.ID, "No"); !errors.Is(err, ErrTransportReadOnly) {
		t.Fatal(err)
	}
	if _, err = b.Mkdir(ctx, "No", ""); !errors.Is(err, ErrTransportReadOnly) {
		t.Fatal(err)
	}
	if err = b.RefreshTablet(ctx); !errors.Is(err, ErrTransportReadOnly) {
		t.Fatal(err)
	}
}

func TestNativeSSHNeverReplaysUnknown(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	t.Setenv("FAKE_SSH_FAILURE", "unknown")
	_, err := b.Mkdir(context.Background(), "Unknown", "")
	var unknown *SSHExecutionUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("unknown error=%T %v", err, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(data) != "call\n" {
		t.Fatalf("replayed: %q", data)
	}
	if !b.RefreshPending() {
		t.Fatal("unknown write must mark pending")
	}
	t.Setenv("FAKE_SSH_FAILURE", "refused")
	_, err = b.List(context.Background())
	var pre *SSHPreExecutionError
	if !errors.As(err, &pre) {
		t.Fatalf("pre-execution=%T %v", err, err)
	}
}

func TestNativeSSHRefreshSafety(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	if _, err := b.Mkdir(context.Background(), "Pending", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.RefreshTablet(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.RefreshPending() {
		t.Fatal("successful refresh still pending")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "systemctl-calls"))
	if strings.Count(string(data), "restart\n") != 1 || strings.Count(string(data), "is-active\n") != 3 {
		t.Fatalf("refresh commands: %s", data)
	}
	t.Setenv("FAKE_XOCHITL_STATUS", "inactive")
	if _, err := b.Mkdir(context.Background(), "More pending", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.RefreshTablet(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing restart") {
		t.Fatalf("inactive refresh: %v", err)
	}
	t.Setenv("FAKE_XOCHITL_STATUS", "active")
	t.Setenv("FAKE_REBOOT", "1")
	if err := b.RefreshTablet(context.Background()); err == nil || !strings.Contains(err.Error(), "rebooted") {
		t.Fatalf("reboot not detected: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "systemctl-calls"))
	if strings.Count(string(data), "restart\n") != 2 {
		t.Fatalf("restart was repeated after reboot: %s", data)
	}
}

func TestNativeSSHPerCallDeferralAndFirstBackup(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	t.Setenv("REMARKABLE_DEFER_RESTART", "")
	ctx := WithDeferredRestart(context.Background(), true)
	d, err := b.Mkdir(ctx, "Deferred", "")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("REMARKABLE_DEFER_RESTART") != "" {
		t.Fatal("per-call deferral changed environment")
	}
	if _, err = os.Stat(filepath.Join(dir, "systemctl-calls")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("per-call write restarted tablet")
	}
	metadata, _ := json.Marshal(transportMetadata("Notebook", "", "DocumentType"))
	files := map[string][]byte{
		"page.rm":             []byte("first edit"),
		"page.rm.bak":         []byte("original ink"),
		"nested/other.rm":     []byte("nested edit"),
		"nested/other.rm.bak": []byte("nested original"),
		d.ID + ".content":     []byte(`{"fileType":"notebook","pages":["page"]}`),
		d.ID + ".metadata":    metadata,
	}
	if err = b.PutDocumentFiles(ctx, d.ID, files); err != nil {
		t.Fatal(err)
	}
	files["page.rm"] = []byte("second edit")
	files["page.rm.bak"] = []byte("must not replace original")
	files["nested/other.rm.bak"] = []byte("must not replace nested original")
	if err = b.PutDocumentFiles(ctx, d.ID, files); err != nil {
		t.Fatal(err)
	}
	got, err := b.Download(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got["page.rm"]) != "second edit" || string(got["page.rm.bak"]) != "original ink" || string(got["nested/other.rm.bak"]) != "nested original" || !bytes.Equal(got[d.ID+".content"], files[d.ID+".content"]) {
		t.Fatalf("first-only backup or sidecars failed: %v", got)
	}
	t.Setenv("REMARKABLE_DEFER_RESTART", "1")
	if err = b.Rename(WithDeferredRestart(context.Background(), false), d.ID, "Env deferred"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "systemctl-calls")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("false context overrode environment deferral")
	}
	t.Setenv("REMARKABLE_DEFER_RESTART", "")
	if err = b.Rename(WithDeferredRestart(context.Background(), false), d.ID, "Not deferred"); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "systemctl-calls"))
	if strings.Count(string(log), "restart\n") != 1 || b.RefreshPending() {
		t.Fatalf("nondeferred operation did not refresh: %s", log)
	}
}

func TestNativeSSHOverwritePreservesInk(t *testing.T) {
	b, dir := fakeTransportSSH(t)
	ctx := context.Background()
	original, err := MarkdownPDF("Original text", "Original")
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := MarkdownPDF("Replacement text", "Replacement")
	if err != nil {
		t.Fatal(err)
	}
	d, err := b.Upload(ctx, original, "pdf", "", "Original title")
	if err != nil {
		t.Fatal(err)
	}
	ink := []byte("preserved ink")
	transportFixture(t, dir, d.ID+"/page.rm", ink)
	content, err := os.ReadFile(filepath.Join(dir, d.ID+".content"))
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Overwrite(ctx, d.ID, replacement); err != nil {
		t.Fatal(err)
	}
	files, err := b.Download(ctx, d.ID)
	if err != nil || !bytes.Equal(files[d.ID+".pdf"], replacement) || !bytes.Equal(files["page.rm"], ink) || !bytes.Equal(files[d.ID+".content"], content) {
		t.Fatalf("overwrite failed: %v", err)
	}
	if err = b.Overwrite(ctx, d.ID, []byte("not PDF")); err == nil {
		t.Fatal("invalid PDF accepted")
	}
	if err = b.Delete(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err = b.Overwrite(ctx, d.ID, original); err == nil {
		t.Fatal("trashed PDF accepted")
	}
}

func TestNativeSSHFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		code              int
		out, stderr, kind string
	}{{255, "", "ssh: connect to host x port 22: Connection refused", "pre"}, {255, "", "Permission denied", "unknown"}, {255, "", sshExecutionMarker + "\nConnection reset", "unknown"}, {1, "", sshExecutionMarker + "\nbad command", "remote"}, {255, "output", "ssh: connect to host x port 22: Connection refused", "unknown"}, {-1, "", sshExecutionMarker, "unknown"}} {
		err := classifySSHFailure(tc.code, []byte(tc.out), []byte(tc.stderr), errors.New("failed"))
		switch tc.kind {
		case "pre":
			var v *SSHPreExecutionError
			if !errors.As(err, &v) {
				t.Fatal(err)
			}
		case "remote":
			var v *SSHRemoteCommandError
			if !errors.As(err, &v) {
				t.Fatal(err)
			}
		default:
			var v *SSHExecutionUnknownError
			if !errors.As(err, &v) {
				t.Fatal(err)
			}
		}
	}
	// Shell quoting preserves bytes without allowing substitutions.
	s := "quote ' \" $(printf injected); *\nmore"
	out, err := exec.Command("sh", "-c", "printf '%s' "+shellQuote(s)).Output()
	if err != nil || string(out) != s {
		t.Fatalf("quote=%q %v", out, err)
	}
}

func TestNativeSelectionPriorityAndNoFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("REMARKABLE_TOKEN", "")
	t.Setenv("REMARKABLE_USE_LOCAL_DIR", "")
	t.Setenv("REMARKABLE_LOCAL_DIR", "")
	t.Setenv("REMARKABLE_USE_SSH", "1")
	t.Setenv("REMARKABLE_USE_USB_WEB", "1")
	t.Setenv("REMARKABLE_DISABLE_CLOUD_FALLBACK", "1")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "[]") }))
	defer s.Close()
	t.Setenv("REMARKABLE_USB_HOST", s.URL)
	b, name, err := SelectBackend()
	if err != nil || name != "usb-web" {
		t.Fatalf("selection=%T %s %v", b, name, err)
	}
	dir := t.TempDir()
	transportFixture(t, dir, "a.metadata", []byte(`{}`))
	t.Setenv("REMARKABLE_LOCAL_DIR", dir)
	b, name, err = SelectBackend()
	if err != nil || name != "local-dir" {
		t.Fatalf("priority=%T %s %v", b, name, err)
	}
	t.Setenv("REMARKABLE_LOCAL_DIR", filepath.Join(dir, "missing"))
	_, name, err = SelectBackend()
	if err == nil || name != "local-dir" {
		t.Fatalf("missing local=%s %v", name, err)
	}
	t.Setenv("REMARKABLE_LOCAL_DIR", "")
	s.Close()
	b, name, err = SelectBackend()
	if err != nil || name != "usb-web" {
		t.Fatalf("unreachable transport not retained: %T %s %v", b, name, err)
	}
}

func TestNativeSelectionCloudFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("REMARKABLE_TOKEN", `{"usertoken":"fixture-only-no-network"}`)
	t.Setenv("REMARKABLE_DISABLE_CLOUD_FALLBACK", "")
	t.Setenv("REMARKABLE_USE_LOCAL_DIR", "1")
	t.Setenv("REMARKABLE_LOCAL_DIR", filepath.Join(t.TempDir(), "missing"))
	b, name, err := SelectBackend()
	if err != nil || name != "cloud" || b == nil {
		t.Fatalf("fallback=%T %s %v", b, name, err)
	}
	t.Setenv("REMARKABLE_DISABLE_CLOUD_FALLBACK", "yes")
	if _, name, err = SelectBackend(); err == nil || name != "local-dir" {
		t.Fatalf("disabled fallback=%s %v", name, err)
	}
}

func TestNativeTransportConfigurationValidation(t *testing.T) {
	t.Setenv("REMARKABLE_USB_MAX_CONCURRENCY", "17")
	if _, err := NewUSBWeb(); err == nil {
		t.Fatal("bad concurrency")
	}
	t.Setenv("REMARKABLE_USB_MAX_CONCURRENCY", "2")
	t.Setenv("REMARKABLE_USB_TIMEOUT", "x")
	if _, err := NewUSBWeb(); err == nil {
		t.Fatal("bad timeout")
	}
	t.Setenv("REMARKABLE_USB_TIMEOUT", "10")
	t.Setenv("REMARKABLE_USB_HOST", "file:///tmp")
	if _, err := NewUSBWeb(); err == nil {
		t.Fatal("bad host")
	}
	t.Setenv("REMARKABLE_SSH_PORT", "0")
	if _, err := NewSSH(); err == nil {
		t.Fatal("bad port")
	}
	t.Setenv("REMARKABLE_SSH_PORT", "22")
	t.Setenv("REMARKABLE_SSH_HOST", "-oProxyCommand=bad")
	if _, err := NewSSH(); err == nil {
		t.Fatal("option injection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	b := &LocalDirBackend{Dir: t.TempDir()}
	transportFixture(t, b.Dir, "a.metadata", []byte(`{}`))
	if _, err := b.List(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !strings.Contains(ErrTransportReadOnly.Error(), "read-only") {
		t.Fatal("sentinel")
	}
}
