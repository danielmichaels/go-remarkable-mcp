package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The fixture independently checks the protocol's two different hash schemes,
// checks upload CRC32C, and never forwards requests to the real cloud.
type syncFixture struct {
	mu                            sync.Mutex
	blobs                         map[string][]byte
	root                          string
	generation                    int64
	commits, puts, gets, renewals int
	conflict                      bool
	failCommit                    int
	malformedCommit               bool
	dropCommitResponse            bool
	rejectToken                   bool
	readFailures                  int
}

func (f *syncFixture) counts() (puts, commits, gets, renewals int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.commits, f.gets, f.renewals
}

func fixtureSHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func fixtureDocHash(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")[1:]
	sort.Slice(lines, func(i, j int) bool { return strings.Split(lines[i], ":")[2] < strings.Split(lines[j], ":")[2] })
	h := sha256.New()
	for _, line := range lines {
		raw, _ := hex.DecodeString(strings.Split(line, ":")[0])
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func newSyncFixture(t *testing.T) (*syncFixture, Backend) {
	t.Helper()
	t.Setenv("REMARKABLE_TOKEN", `{"devicetoken":"device-fixture","usertoken":"initial-fixture"}`)
	t.Setenv("REMARKABLE_CACHE_DIR", t.TempDir())
	t.Setenv("REMARKABLE_DISABLE_CACHE", "")
	t.Setenv("REMARKABLE_CACHE_MAX_BLOB", "4194304")
	t.Setenv("REMARKABLE_RETRY_DELAY", "0")
	t.Setenv("REMARKABLE_RETRY_ATTEMPTS", "3")
	root := []byte("4\n0:.:0:0\n")
	f := &syncFixture{blobs: map[string][]byte{fixtureSHA(root): root}, root: fixtureSHA(root), generation: 7}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/auth" {
			f.renewals++
			if r.Header.Get("Authorization") != "Bearer device-fixture" {
				t.Error("incorrect device token")
			}
			io.WriteString(w, "renewed-fixture")
			return
		}
		if f.rejectToken && r.Header.Get("Authorization") != "Bearer renewed-fixture" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/root" && r.Method == "GET":
			if f.readFailures > 0 {
				f.readFailures--
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"hash": f.root, "generation": f.generation})
		case r.URL.Path == "/commit" && r.Method == "PUT":
			f.commits++
			var v struct {
				Hash       string `json:"hash"`
				Generation int64  `json:"generation"`
				Broadcast  bool   `json:"broadcast"`
			}
			if json.NewDecoder(r.Body).Decode(&v) != nil {
				t.Error("bad commit JSON")
				w.WriteHeader(400)
				return
			}
			if !v.Broadcast || r.Header.Get("rm-filename") != "roothash" {
				t.Error("missing commit headers or broadcast")
			}
			if f.conflict {
				f.conflict = false
				f.generation++
				w.WriteHeader(409)
				return
			}
			if v.Generation != f.generation {
				t.Errorf("generation: got %d want %d", v.Generation, f.generation)
				w.WriteHeader(412)
				return
			}
			if f.failCommit != 0 {
				w.WriteHeader(f.failCommit)
				return
			}
			body, ok := f.blobs[v.Hash]
			if !ok || !bytes.HasPrefix(body, []byte("4\n0:.:")) {
				t.Error("commit missing schema 4 root")
				w.WriteHeader(400)
				return
			}
			f.root = v.Hash
			f.generation++
			if f.dropCommitResponse {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
			if f.malformedCommit {
				io.WriteString(w, `{"hash":"wrong"}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"hash": f.root, "generation": f.generation})
		case strings.HasPrefix(r.URL.Path, "/files/"):
			h := strings.TrimPrefix(r.URL.Path, "/files/")
			if r.Method == "GET" {
				f.gets++
				b, ok := f.blobs[h]
				if !ok {
					w.WriteHeader(404)
					return
				}
				w.Write(b)
				return
			}
			if r.Method != "PUT" {
				w.WriteHeader(405)
				return
			}
			f.puts++
			b, _ := io.ReadAll(r.Body)
			want := fixtureSHA(b)
			if strings.HasSuffix(r.Header.Get("rm-filename"), ".docSchema") && bytes.HasPrefix(b, []byte("3\n")) {
				want = fixtureDocHash(b)
			}
			if h != want {
				t.Errorf("incorrect blob address %s; want %s", h, want)
				w.WriteHeader(400)
				return
			}
			var crc [4]byte
			binary.BigEndian.PutUint32(crc[:], crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
			if r.Header.Get("x-goog-hash") != "crc32c="+base64.StdEncoding.EncodeToString(crc[:]) {
				t.Error("incorrect CRC32C")
			}
			f.blobs[h] = b
			w.WriteHeader(200)
		default:
			t.Errorf("unexpected fixture request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	b, e := NewCloud()
	if e != nil {
		t.Fatal(e)
	}
	c := b.(*nativeCloud)
	c.http = s.Client()
	c.auth = s.URL + "/auth"
	c.root = s.URL + "/root"
	c.rootPut = s.URL + "/commit"
	c.files = s.URL + "/files/"
	return f, b
}
func requireSyncOK(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}

func TestNativeCloudLifecycleAndCache(t *testing.T) {
	f, b := newSyncFixture(t)
	ctx := context.Background()
	folder, e := b.Mkdir(ctx, "Folder", "")
	requireSyncOK(t, e)
	d, e := b.Upload(ctx, []byte("fixture epub bytes"), ".EPUB", folder.ID, "Book")
	requireSyncOK(t, e)
	docs, e := b.List(ctx)
	requireSyncOK(t, e)
	if len(docs) != 2 {
		t.Fatalf("documents: %+v", docs)
	}
	files, e := b.Download(ctx, d.ID)
	requireSyncOK(t, e)
	if len(files) != 4 || string(files[d.ID+".epub"]) != "fixture epub bytes" {
		t.Fatalf("download: %v", files)
	}
	var layout map[string]any
	requireSyncOK(t, json.Unmarshal(files[d.ID+".content"], &layout))
	if layout["pageCount"] != float64(0) || len(layout["pages"].([]any)) != 0 {
		t.Fatal("EPUB must reflow without fixed pages")
	}
	_, _, before, _ := f.counts()
	b.Refresh()
	again, e := b.Download(ctx, d.ID)
	requireSyncOK(t, e)
	_, _, gets, _ := f.counts()
	if gets != before || !bytes.Equal(again[d.ID+".content"], files[d.ID+".content"]) {
		t.Fatal("immutable disk cache not reused")
	}
	// Corrupt cache bytes must not silently become document content.
	cache := b.(*nativeCloud).cache
	requireSyncOK(t, os.WriteFile(filepath.Join(cache, fixtureSHA(files[d.ID+".epub"])), []byte("corrupt"), 0600))
	_, e = b.Download(ctx, d.ID)
	requireSyncOK(t, e)
	_, _, gets, _ = f.counts()
	if gets != before+1 {
		t.Fatal("corrupt cache was not refetched")
	}
	requireSyncOK(t, b.Rename(ctx, d.ID, "Renamed"))
	requireSyncOK(t, b.Move(ctx, d.ID, ""))
	requireSyncOK(t, b.Delete(ctx, d.ID))
	docs, e = b.List(ctx)
	requireSyncOK(t, e)
	for _, v := range docs {
		if v.ID == d.ID && (v.Name != "Renamed" || v.Parent != "trash" || v.FileType != "epub") {
			t.Fatalf("wrong metadata: %+v", v)
		}
	}
}

// A small but structurally valid PDF, with real object offsets and xref table.
func fixturePDF(pages int, label string) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	kids := []string{}
	for i := 0; i < pages; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", len(objects)+1))
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << >> >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", pages, strings.Join(kids, " "))
	var b bytes.Buffer
	fmt.Fprintf(&b, "%%PDF-1.4\n%%%s\n", label)
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, n := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", n)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return b.Bytes()
}
func TestNativeOverwritePreservesLayoutAndIdentity(t *testing.T) {
	f, b := newSyncFixture(t)
	ctx := context.Background()
	original := fixturePDF(2, "original")
	replacement := fixturePDF(2, "replacement")
	d, e := b.Upload(ctx, original, "pdf", "folder-id", "Original title")
	requireSyncOK(t, e)
	// Seed a real tablet sidecar into the remote document, independently of the client.
	f.mu.Lock()
	rootLines := strings.Split(strings.TrimSpace(string(f.blobs[f.root])), "\n")
	entry := strings.Split(rootLines[2], ":")
	index := f.blobs[entry[0]]
	sidecar := []byte("annotations survive")
	sideHash := fixtureSHA(sidecar)
	f.blobs[sideHash] = sidecar
	index = append(append([]byte(nil), index...), []byte(fmt.Sprintf("%s:0:%s/annotation.rm:0:%d\n", sideHash, d.ID, len(sidecar)))...)
	indexHash := fixtureDocHash(index)
	f.blobs[indexHash] = index
	entry[0] = indexHash
	entry[3] = "5"
	size, _ := strconv.Atoi(entry[4])
	entry[4] = strconv.Itoa(size + len(sidecar))
	rootBody := []byte(fmt.Sprintf("4\n0:.:1:%s\n%s\n", entry[4], strings.Join(entry, ":")))
	f.root = fixtureSHA(rootBody)
	f.blobs[f.root] = rootBody
	f.generation++
	f.mu.Unlock()
	before, e := b.Download(ctx, d.ID)
	requireSyncOK(t, e)
	f.mu.Lock()
	f.conflict = true
	f.mu.Unlock()
	requireSyncOK(t, b.Overwrite(ctx, d.ID, replacement))
	after, e := b.Download(ctx, d.ID)
	requireSyncOK(t, e)
	if len(before) != len(after) {
		t.Fatal("overwrite changed file set")
	}
	for name, data := range before {
		if name != d.ID+".pdf" && name != d.ID+".metadata" && !bytes.Equal(data, after[name]) {
			t.Errorf("overwrite changed %s", name)
		}
	}
	if !bytes.Equal(after[d.ID+".pdf"], replacement) {
		t.Fatal("PDF not replaced")
	}
	var oldMeta, newMeta map[string]any
	json.Unmarshal(before[d.ID+".metadata"], &oldMeta)
	json.Unmarshal(after[d.ID+".metadata"], &newMeta)
	for k, v := range oldMeta {
		if k != "version" && k != "lastModified" && k != "metadatamodified" && fmt.Sprint(newMeta[k]) != fmt.Sprint(v) {
			t.Errorf("metadata %s changed", k)
		}
	}
	if newMeta["version"] != oldMeta["version"].(float64)+1 {
		t.Fatal("version not bumped exactly once after conflict")
	}
	puts, commits, _, _ := f.counts()
	if e = b.Overwrite(ctx, d.ID, fixturePDF(1, "wrong pages")); e == nil {
		t.Fatal("accepted different page count")
	}
	newPuts, newCommits, _, _ := f.counts()
	if newPuts != puts || newCommits != commits {
		t.Fatal("invalid overwrite performed writes")
	}
	requireSyncOK(t, b.Delete(ctx, d.ID))
	puts, commits, _, _ = f.counts()
	if e = b.Overwrite(ctx, d.ID, replacement); e == nil {
		t.Fatal("accepted trashed PDF")
	}
	newPuts, newCommits, _, _ = f.counts()
	if newPuts != puts || newCommits != commits {
		t.Fatal("trashed overwrite performed writes")
	}
}

func TestNativeCommitDoesNotRetryAmbiguousOutcomes(t *testing.T) {
	for _, mode := range []string{"server-error", "invalid-ack", "connection-loss-after-commit"} {
		t.Run(mode, func(t *testing.T) {
			f, b := newSyncFixture(t)
			if mode == "server-error" {
				f.failCommit = 503
			} else if mode == "invalid-ack" {
				f.malformedCommit = true
			} else {
				f.dropCommitResponse = true
			}
			_, e := b.Mkdir(context.Background(), "Folder", "")
			if e == nil {
				t.Fatal("expected uncertain commit error")
			}
			_, commits, _, _ := f.counts()
			if commits != 1 {
				t.Fatalf("root commit retried %d times", commits)
			}
			if mode != "server-error" {
				docs, err := b.List(context.Background())
				requireSyncOK(t, err)
				if len(docs) != 1 {
					t.Fatalf("fixture commit was not applied exactly once: %+v", docs)
				}
			}
		})
	}
}

func TestNativeTokenRenewalAndSafeReadRetry(t *testing.T) {
	f, b := newSyncFixture(t)
	f.rejectToken = true
	f.readFailures = 1
	docs, e := b.List(context.Background())
	requireSyncOK(t, e)
	_, _, _, renewals := f.counts()
	if len(docs) != 0 || renewals != 1 {
		t.Fatalf("unexpected renewal/list: %d %+v", renewals, docs)
	}
	_, e = b.Upload(context.Background(), []byte("not PDF"), "pdf", "", "invalid")
	if e == nil {
		t.Fatal("invalid PDF accepted")
	}
	puts, commits, _, _ := f.counts()
	if puts != 0 || commits != 0 {
		t.Fatal("invalid PDF caused writes")
	}
}
