package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Document is cloud metadata. Modified is an RFC3339 timestamp, when available.
type Document struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Parent   string   `json:"parent"`
	Type     string   `json:"type"`
	Modified string   `json:"modified"`
	Tags     []string `json:"tags"`
	FileType string   `json:"fileType"`
}

type Backend interface {
	List(context.Context) ([]Document, error)
	Download(context.Context, string) (map[string][]byte, error)
	Upload(context.Context, []byte, string, string, string) (Document, error)
	Overwrite(context.Context, string, []byte) error
	Mkdir(context.Context, string, string) (Document, error)
	Move(context.Context, string, string) error
	Rename(context.Context, string, string) error
	Delete(context.Context, string) error
	Refresh()
}

func Enabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("UUID entropy unavailable: " + err.Error())
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// nativeCloud owns its authentication and protocol state; it does not use the
// old cloud client or any subprocess. A lock serializes token changes and writes.
type nativeCloud struct {
	mu                                              sync.Mutex
	http                                            *http.Client
	device, user, auth, root, rootPut, files, cache string
}

func NewCloud() (Backend, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(os.Getenv("REMARKABLE_TOKEN"))
	if token == "" {
		b, e := os.ReadFile(filepath.Join(home, ".rmapi"))
		if e != nil {
			return nil, errors.New("set REMARKABLE_TOKEN or configure ~/.rmapi")
		}
		token = strings.TrimSpace(string(b))
	}
	c := &nativeCloud{http: &http.Client{Timeout: 120 * time.Second}, auth: "https://webapp-prod.cloud.remarkable.engineering/token/json/2/user/new", root: "https://internal.cloud.remarkable.com/sync/v4/root", rootPut: "https://internal.cloud.remarkable.com/sync/v3/root", files: "https://internal.cloud.remarkable.com/sync/v3/files/", cache: filepath.Join(home, ".remarkable", "cache", "blobs")}
	if v := os.Getenv("REMARKABLE_CACHE_DIR"); v != "" {
		c.cache = v
	}
	if strings.HasPrefix(token, "{") {
		var t struct {
			Device string `json:"devicetoken"`
			User   string `json:"usertoken"`
		}
		if err := json.Unmarshal([]byte(token), &t); err != nil {
			return nil, errors.New("invalid token JSON")
		}
		c.device, c.user = t.Device, t.User
	} else if strings.HasPrefix(token, "eyJ") {
		c.device = token
	} else {
		return nil, errors.New("expected token JSON or device JWT")
	}
	if c.device == "" && c.user == "" {
		return nil, errors.New("token contains no credentials")
	}
	return c, nil
}

// Refresh has no disk-cache side effects: immutable blobs remain valid forever.
// Root and document state is deliberately read afresh on each operation.
func (c *nativeCloud) Refresh() {}

func nativeWait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func nativeRetryDelay(h string, attempt int) time.Duration {
	base := 2.0
	if n, e := strconv.ParseFloat(os.Getenv("REMARKABLE_RETRY_DELAY"), 64); e == nil && n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
		base = n
	}
	d := time.Duration(mathrand.Float64() * math.Min(base*math.Pow(2, float64(attempt)), 20) * float64(time.Second))
	if n, e := strconv.ParseFloat(h, 64); e == nil && n >= 0 && n < 1e9 {
		d = time.Duration(n * float64(time.Second))
	} else if t, e := http.ParseTime(h); e == nil && time.Until(t) >= 0 {
		d = time.Until(t)
	}
	if d > 20*time.Second {
		d = 20 * time.Second
	}
	return d
}

// Only immutable blob writes and reads may be retried after an uncertain outcome.
// A root PUT is never retried on a timeout, transport failure, or 5xx.
func (c *nativeCloud) raw(ctx context.Context, method, url, token string, body []byte, headers map[string]string, safe bool) ([]byte, int, error) {
	attempts := 1
	if safe {
		attempts = 3
		if n, e := strconv.Atoi(os.Getenv("REMARKABLE_RETRY_ATTEMPTS")); e == nil && n > 0 {
			attempts = n
		}
	}
	for i := 0; i < attempts; i++ {
		req, e := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if e != nil {
			return nil, 0, e
		}
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, e := c.http.Do(req)
		status := 0
		var data []byte
		retryAfter := ""
		if e == nil {
			status = resp.StatusCode
			retryAfter = resp.Header.Get("Retry-After")
			data, e = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		retryable := e != nil || status == 429 || status == 500 || status == 502 || status == 503 || status == 504
		if retryable && i+1 < attempts {
			if e := nativeWait(ctx, nativeRetryDelay(retryAfter, i)); e != nil {
				return nil, status, e
			}
			continue
		}
		if e != nil {
			return nil, status, fmt.Errorf("cloud request failed (outcome may be unknown): %w", e)
		}
		if status < 200 || status >= 300 {
			return data, status, fmt.Errorf("cloud HTTP %d", status)
		}
		return data, status, nil
	}
	return nil, 0, errors.New("retry exhausted")
}
func (c *nativeCloud) renewNative(ctx context.Context) error {
	if c.device == "" {
		return errors.New("no device token available for renewal")
	}
	b, _, e := c.raw(ctx, "POST", c.auth, c.device, nil, nil, true)
	if e != nil {
		return e
	}
	c.user = strings.TrimSpace(string(b))
	if c.user == "" {
		return errors.New("empty user token")
	}
	return nil
}
func (c *nativeCloud) call(ctx context.Context, method, url string, body []byte, headers map[string]string, safe bool) ([]byte, int, error) {
	if c.user == "" {
		if e := c.renewNative(ctx); e != nil {
			return nil, 0, e
		}
	}
	b, s, e := c.raw(ctx, method, url, c.user, body, headers, safe)
	if s == 401 {
		if e := c.renewNative(ctx); e != nil {
			return nil, s, e
		}
		return c.raw(ctx, method, url, c.user, body, headers, safe)
	}
	return b, s, e
}

type nativeEntry struct {
	Hash, ID       string
	Subfiles, Size int64
}

func nativeSHA(b []byte) string     { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func nativeValidHash(h string) bool { b, e := hex.DecodeString(h); return e == nil && len(b) == 32 }
func nativeParse(b []byte) ([]nativeEntry, error) {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 || (lines[0] != "3" && lines[0] != "4") {
		return nil, errors.New("unsupported sync index schema")
	}
	out := []nativeEntry{}
	for i, line := range lines[1:] {
		p := strings.Split(line, ":")
		if lines[0] == "4" && i == 0 && len(p) == 4 && p[0] == "0" && p[1] != "" {
			count, countErr := strconv.ParseInt(p[2], 10, 64)
			size, sizeErr := strconv.ParseInt(p[3], 10, 64)
			if countErr != nil || sizeErr != nil || count < 0 || size < 0 {
				return nil, errors.New("invalid sync index header")
			}
			continue
		}
		if len(p) != 5 || !nativeValidHash(p[0]) || p[2] == "" {
			return nil, errors.New("invalid sync index entry")
		}
		n, e := strconv.ParseInt(p[3], 10, 64)
		if e != nil || n < 0 {
			return nil, errors.New("invalid subfile count")
		}
		size, e := strconv.ParseInt(p[4], 10, 64)
		if e != nil || size < 0 {
			return nil, errors.New("invalid blob size")
		}
		out = append(out, nativeEntry{p[0], p[2], n, size})
	}
	seen := map[string]bool{}
	for _, v := range out {
		if seen[v.ID] {
			return nil, errors.New("duplicate index filename")
		}
		seen[v.ID] = true
	}
	return out, nil
}
func nativeOrdered(es []nativeEntry) []nativeEntry {
	es = append([]nativeEntry(nil), es...)
	sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })
	return es
}
func nativeIndexHash(es []nativeEntry) string {
	h := sha256.New()
	for _, e := range nativeOrdered(es) {
		b, _ := hex.DecodeString(e.Hash)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func nativeIndex(es []nativeEntry, root bool) []byte {
	var b strings.Builder
	if root {
		var total int64
		for _, e := range es {
			total += e.Size
		}
		fmt.Fprintf(&b, "4\n0:.:%d:%d\n", len(es), total)
	} else {
		b.WriteString("3\n")
	}
	for _, e := range nativeOrdered(es) {
		fmt.Fprintf(&b, "%s:0:%s:%d:%d\n", e.Hash, e.ID, e.Subfiles, e.Size)
	}
	return []byte(b.String())
}
func nativeIntegrity(b []byte, hash, name string) bool {
	if strings.HasSuffix(name, ".docSchema") && bytes.HasPrefix(b, []byte("3\n")) {
		es, e := nativeParse(b)
		return e == nil && nativeIndexHash(es) == hash
	}
	return nativeSHA(b) == hash
}
func (c *nativeCloud) cacheWrite(hash string, b []byte) {
	max := 4 << 20
	if n, e := strconv.Atoi(os.Getenv("REMARKABLE_CACHE_MAX_BLOB")); e == nil {
		max = n
	}
	if Enabled("REMARKABLE_DISABLE_CACHE") || len(b) > max {
		return
	}
	if os.MkdirAll(c.cache, 0700) != nil {
		return
	}
	f, e := os.CreateTemp(c.cache, ".blob-*")
	if e != nil {
		return
	}
	name := f.Name()
	defer os.Remove(name)
	_, e = f.Write(b)
	closeErr := f.Close()
	if e == nil && closeErr == nil {
		_ = os.Rename(name, filepath.Join(c.cache, hash))
	}
}
func (c *nativeCloud) get(ctx context.Context, hash, name string) ([]byte, error) {
	if !nativeValidHash(hash) {
		return nil, errors.New("invalid blob hash")
	}
	if !Enabled("REMARKABLE_DISABLE_CACHE") {
		if b, e := os.ReadFile(filepath.Join(c.cache, hash)); e == nil && nativeIntegrity(b, hash, name) {
			return b, nil
		}
	}
	b, _, e := c.call(ctx, "GET", c.files+hash, nil, map[string]string{"rm-filename": name}, true)
	if e != nil {
		return nil, e
	}
	if !nativeIntegrity(b, hash, name) {
		return nil, errors.New("blob hash mismatch")
	}
	c.cacheWrite(hash, b)
	return b, nil
}
func (c *nativeCloud) put(ctx context.Context, b []byte, hash, name string) error {
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli)))
	_, _, e := c.call(ctx, "PUT", c.files+hash, b, map[string]string{"rm-filename": name, "Content-Type": "application/octet-stream", "x-goog-hash": "crc32c=" + base64.StdEncoding.EncodeToString(crc[:])}, true)
	if e == nil {
		c.cacheWrite(hash, b)
	}
	return e
}
func (c *nativeCloud) putFile(ctx context.Context, name string, b []byte) (nativeEntry, error) {
	e := nativeEntry{Hash: nativeSHA(b), ID: name, Size: int64(len(b))}
	return e, c.put(ctx, b, e.Hash, name)
}
func (c *nativeCloud) putIndex(ctx context.Context, id string, files []nativeEntry) (nativeEntry, error) {
	e := nativeEntry{Hash: nativeIndexHash(files), ID: id, Subfiles: int64(len(files))}
	for _, f := range files {
		e.Size += f.Size
	}
	return e, c.put(ctx, nativeIndex(files, false), e.Hash, id+".docSchema")
}
func (c *nativeCloud) readRoot(ctx context.Context) ([]nativeEntry, int64, error) {
	b, _, e := c.call(ctx, "GET", c.root, nil, nil, true)
	if e != nil {
		return nil, 0, e
	}
	var r struct {
		Hash       string `json:"hash"`
		Generation *int64 `json:"generation"`
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return nil, 0, e
	}
	if r.Generation == nil || *r.Generation < 0 {
		return nil, 0, errors.New("root missing valid generation")
	}
	b, e = c.get(ctx, r.Hash, "root.docSchema")
	if e != nil {
		return nil, 0, e
	}
	es, e := nativeParse(b)
	return es, *r.Generation, e
}
func (c *nativeCloud) docFiles(ctx context.Context, e nativeEntry) ([]nativeEntry, error) {
	b, err := c.get(ctx, e.Hash, e.ID+".docSchema")
	if err != nil {
		return nil, err
	}
	return nativeParse(b)
}
func nativeFind(es []nativeEntry, id string) (int, error) {
	for i, e := range es {
		if e.ID == id {
			return i, nil
		}
	}
	return 0, fmt.Errorf("document or file %q not found", id)
}
func (c *nativeCloud) jsonFile(ctx context.Context, files []nativeEntry, name string) (map[string]any, int, error) {
	i, e := nativeFind(files, name)
	if e != nil {
		return nil, 0, e
	}
	b, e := c.get(ctx, files[i].Hash, name)
	if e != nil {
		return nil, 0, e
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	e = dec.Decode(&m)
	if e == nil && m == nil {
		e = errors.New("expected JSON object")
	}
	return m, i, e
}
func nativeText(m map[string]any, key string) string { s, _ := m[key].(string); return s }
func nativeModified(value any) string {
	ms, err := strconv.ParseInt(fmt.Sprint(value), 10, 64)
	if err != nil {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}
func nativeTags(m map[string]any) []string {
	out := []string{}
	if a, ok := m["tags"].([]any); ok {
		for _, v := range a {
			if s, ok := v.(string); ok {
				out = append(out, s)
			} else if t, ok := v.(map[string]any); ok {
				if s := nativeText(t, "name"); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}
func (c *nativeCloud) List(ctx context.Context) ([]Document, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	es, _, e := c.readRoot(ctx)
	if e != nil {
		return nil, e
	}
	out := []Document{}
	for _, v := range es {
		fs, e := c.docFiles(ctx, v)
		if e != nil {
			return nil, e
		}
		m, _, e := c.jsonFile(ctx, fs, v.ID+".metadata")
		if e != nil {
			return nil, e
		}
		if m["deleted"] == true {
			continue
		}
		d := Document{ID: v.ID, Name: nativeText(m, "visibleName"), Parent: nativeText(m, "parent"), Type: nativeText(m, "type"), Modified: nativeModified(m["lastModified"]), Tags: nativeTags(m)}
		if d.Name == "" {
			d.Name = v.ID
		}
		if d.Type == "" {
			d.Type = "DocumentType"
		}
		if _, err := nativeFind(fs, v.ID+".content"); err == nil {
			cm, _, err := c.jsonFile(ctx, fs, v.ID+".content")
			if err != nil {
				return nil, err
			}
			d.FileType = nativeText(cm, "fileType")
			if len(d.Tags) == 0 {
				d.Tags = nativeTags(cm)
			}
		}
		// Original source blobs take precedence over a stale content fileType.
		for _, f := range fs {
			if strings.HasSuffix(f.ID, ".pdf") {
				d.FileType = "pdf"
				break
			}
			if strings.HasSuffix(f.ID, ".epub") {
				d.FileType = "epub"
				break
			}
		}
		if d.FileType == "" && d.Type != "CollectionType" {
			d.FileType = "notebook"
		}
		out = append(out, d)
	}
	return out, nil
}
func (c *nativeCloud) Download(ctx context.Context, id string) (map[string][]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	es, _, e := c.readRoot(ctx)
	if e != nil {
		return nil, e
	}
	i, e := nativeFind(es, id)
	if e != nil {
		return nil, e
	}
	fs, e := c.docFiles(ctx, es[i])
	if e != nil {
		return nil, e
	}
	out := map[string][]byte{}
	for _, f := range fs {
		b, e := c.get(ctx, f.Hash, f.ID)
		if e != nil {
			return nil, e
		}
		out[f.ID] = b
	}
	return out, nil
}
func (c *nativeCloud) commit(ctx context.Context, mutate func([]nativeEntry) ([]nativeEntry, error)) error {
	for attempt := 0; attempt < 5; attempt++ {
		es, g, e := c.readRoot(ctx)
		if e != nil {
			return e
		}
		es, e = mutate(es)
		if e != nil {
			return e
		}
		body := nativeIndex(es, true)
		h := nativeSHA(body)
		if e = c.put(ctx, body, h, "root.docSchema"); e != nil {
			return e
		}
		b, _ := json.Marshal(map[string]any{"hash": h, "generation": g, "broadcast": true})
		reply, status, e := c.call(ctx, "PUT", c.rootPut, b, map[string]string{"rm-filename": "roothash", "Content-Type": "application/json"}, false)
		if status == 409 || status == 412 || status == 428 {
			if attempt < 4 {
				if e := nativeWait(ctx, nativeRetryDelay("", attempt)); e != nil {
					return e
				}
				continue
			}
			return errors.New("root generation conflict after five attempts")
		}
		if e != nil {
			return fmt.Errorf("root commit not confirmed; not retrying: %w", e)
		}
		var r struct {
			Hash string `json:"hash"`
		}
		if json.Unmarshal(reply, &r) != nil || r.Hash != h {
			return errors.New("root commit not confirmed: invalid or mismatched response; not retrying")
		}
		return nil
	}
	return errors.New("root commit exhausted")
}
func nativeBump(m map[string]any) {
	n := int64(1)
	if v, ok := m["version"]; ok {
		if i, e := strconv.ParseInt(fmt.Sprint(v), 10, 64); e == nil {
			n = i
		}
	}
	m["version"] = n + 1
	m["lastModified"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
	m["metadatamodified"] = true
}
func (c *nativeCloud) change(ctx context.Context, id string, apply func(map[string]any)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commit(ctx, func(es []nativeEntry) ([]nativeEntry, error) {
		i, e := nativeFind(es, id)
		if e != nil {
			return nil, e
		}
		fs, e := c.docFiles(ctx, es[i])
		if e != nil {
			return nil, e
		}
		m, j, e := c.jsonFile(ctx, fs, id+".metadata")
		if e != nil {
			return nil, e
		}
		apply(m)
		nativeBump(m)
		b, e := json.Marshal(m)
		if e != nil {
			return nil, e
		}
		fs[j], e = c.putFile(ctx, fs[j].ID, b)
		if e != nil {
			return nil, e
		}
		es[i], e = c.putIndex(ctx, id, fs)
		return es, e
	})
}
func (c *nativeCloud) Move(ctx context.Context, id, parent string) error {
	return c.change(ctx, id, func(m map[string]any) { m["parent"] = parent })
}
func (c *nativeCloud) Rename(ctx context.Context, id, name string) error {
	return c.change(ctx, id, func(m map[string]any) { m["visibleName"] = name })
}
func (c *nativeCloud) Delete(ctx context.Context, id string) error { return c.Move(ctx, id, "trash") }
func (c *nativeCloud) create(ctx context.Context, d Document, files map[string][]byte) (Document, error) {
	fs := []nativeEntry{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, e := c.putFile(ctx, name, files[name])
		if e != nil {
			return Document{}, e
		}
		fs = append(fs, f)
	}
	entry, e := c.putIndex(ctx, d.ID, fs)
	if e != nil {
		return Document{}, e
	}
	e = c.commit(ctx, func(es []nativeEntry) ([]nativeEntry, error) {
		for _, v := range es {
			if v.ID == d.ID {
				return nil, errors.New("new UUID already exists")
			}
		}
		return append(es, entry), nil
	})
	if e != nil {
		return Document{}, e
	}
	return d, nil
}
func (c *nativeCloud) Mkdir(ctx context.Context, name, parent string) (Document, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := NewID()
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	m := map[string]any{"createdTime": now, "lastModified": now, "new": false, "parent": parent, "pinned": false, "source": "", "type": "CollectionType", "visibleName": name}
	b, _ := json.Marshal(m)
	return c.create(ctx, Document{ID: id, Name: name, Parent: parent, Type: "CollectionType", Modified: nativeModified(now)}, map[string][]byte{id + ".metadata": b})
}
func (c *nativeCloud) Upload(ctx context.Context, b []byte, ext, parent, name string) (Document, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ext = strings.TrimPrefix(strings.ToLower(ext), ".")
	if ext != "pdf" && ext != "epub" {
		return Document{}, errors.New("upload requires pdf or epub")
	}
	count := 0
	if ext == "pdf" {
		var e error
		count, e = PDFPageCount(b)
		if e != nil {
			return Document{}, e
		}
		if count <= 0 {
			return Document{}, errors.New("PDF has no pages")
		}
	}
	id := NewID()
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	pages := []string{}
	for i := 0; i < count; i++ {
		pages = append(pages, NewID())
	}
	cm := map[string]any{"coverPageNumber": -1, "documentMetadata": map[string]any{}, "dummyDocument": false, "extraMetadata": map[string]any{}, "fileType": ext, "fontName": "", "formatVersion": 1, "lineHeight": -1, "margins": 100, "orientation": "portrait", "pageCount": count, "pageTags": []any{}, "pages": pages, "tags": []any{}, "textScale": 1}
	m := map[string]any{"createdTime": now, "deleted": false, "lastModified": now, "lastOpened": now, "lastOpenedPage": 0, "metadatamodified": false, "modified": false, "new": false, "parent": parent, "pinned": false, "source": "", "synced": false, "type": "DocumentType", "version": 1, "visibleName": name}
	mb, _ := json.Marshal(m)
	cb, _ := json.Marshal(cm)
	pd := "\n"
	if count > 0 {
		pd = strings.Repeat("Blank\n", count)
	}
	return c.create(ctx, Document{ID: id, Name: name, Parent: parent, Type: "DocumentType", Modified: nativeModified(now), FileType: ext}, map[string][]byte{id + ".metadata": mb, id + ".content": cb, id + ".pagedata": []byte(pd), id + "." + ext: b})
}
func (c *nativeCloud) Overwrite(ctx context.Context, id string, b []byte) error {
	count, e := PDFPageCount(b)
	if e != nil {
		return e
	}
	if count <= 0 {
		return errors.New("PDF has no pages")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commit(ctx, func(es []nativeEntry) ([]nativeEntry, error) {
		i, e := nativeFind(es, id)
		if e != nil {
			return nil, e
		}
		fs, e := c.docFiles(ctx, es[i])
		if e != nil {
			return nil, e
		}
		p, e := nativeFind(fs, id+".pdf")
		if e != nil {
			return nil, errors.New("overwrite requires an existing PDF")
		}
		layout, _, e := c.jsonFile(ctx, fs, id+".content")
		if e != nil {
			return nil, e
		}
		if fmt.Sprint(layout["pageCount"]) != strconv.Itoa(count) {
			return nil, errors.New("in-place PDF overwrite requires the same page count")
		}
		m, j, e := c.jsonFile(ctx, fs, id+".metadata")
		if e != nil {
			return nil, e
		}
		if m["deleted"] == true || m["parent"] == "trash" {
			return nil, errors.New("cannot overwrite a deleted or trashed document")
		}
		nativeBump(m)
		mb, e := json.Marshal(m)
		if e != nil {
			return nil, e
		}
		fs[p], e = c.putFile(ctx, id+".pdf", b)
		if e != nil {
			return nil, e
		}
		fs[j], e = c.putFile(ctx, id+".metadata", mb)
		if e != nil {
			return nil, e
		}
		es[i], e = c.putIndex(ctx, id, fs)
		return es, e
	})
}
