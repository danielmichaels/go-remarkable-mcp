# go-remarkable-mcp

A native Go MCP server for reMarkable tablets, based on the public contracts of
[remarkable-mcp](https://github.com/SamMorrowDrums/remarkable-mcp) 2.1.0. There is
no Python runtime, interpreter, rendering worker, or Python fallback. PDF/EPUB
processing uses compiled MuPDF through go-fitz. Optional local OCR invokes the
native Tesseract executable. SSH uses the system SSH client.

**Experimental.** This is not a certified 1:1 replacement. Real-device write,
wake, and restart validation is still needed. Start with read-only access and
keep a backup before enabling writes. This project is not affiliated with
reMarkable AS. Publishing this source does not install or configure an MCP client.

Licensed under [AGPL-3.0](LICENSE), with retained upstream MIT and font notices.

## Implemented

- Official Go MCP SDK v1.7.0: stdio, Streamable HTTP, tools, resources,
  completions, six prompts, and the existing interactive canvas HTML.
- Captured Python tool input/output schemas and defaults in `contracts.json`.
  Tools are selected by transport and read-only mode. Captured JSON output
  schemas retain their JSON-string `result` wrapper.
- Cloud, local-directory, USB web, and SSH transports. Local-directory mode
  always remains read-only. Root-scoped browsing, tags, recent/search/read,
  grep, original PDF/EPUB text, image/canvas, and PDF/Markdown exports.
- Native v3-v6 stroke parsing, v6 typed-text CRDT ordering, glyph highlights,
  mapped PDF underlays, inserted blank pages, and physical-page export order.
- Native Markdown-to-PDF, uploads, folders, move/rename/trash, and confirmed
  deletion. Permanent deletion is limited to SSH and exact enumerated targets.
- In-process MuPDF Story layout with the Python stylesheet and bundled CJK and
  Arabic fallback fonts; EPUB extraction in manifest order with native HTML
  parsing. Markdown links remain clickable, including wrapped links and internal
  anchors. No Python layout or text-extraction process is used.
- Native append-only SSH authoring, original-page backups, notebook creation,
  per-call restart deferral, and explicit tablet refresh.
- FIFO SSH scheduling, cancellation, bounded pre-execution recovery, coalesced
  restart/readiness checks, and bounded shutdown draining. Unknown write outcomes
  never replay automatically; multi-file mutations can still partially persist.
- Python-compatible lookaround and named backreferences for grep, with a bounded
  match timeout. Public response comparisons cover Unicode content pagination,
  browse/read/search, status, image metadata, and Markdown export bytes.
- Native Tesseract OCR and Google Vision REST OCR using an API key or Google
  Application Default Credentials. Google errors can fall back to Tesseract.
- Disk-backed temporary exports: 15-minute expiry, at most eight retained,
  cleanup on further exports and graceful close; no cleanup polling loop.

Cloud authentication reads `REMARKABLE_TOKEN` or `~/.rmapi`. User tokens renew
after HTTP 401. Content-addressed blobs are verified and cached atomically.
Root commits use generations. Only explicit generation conflicts replay root
writes; a connection loss or ambiguous commit acknowledgement returns an error
without replay. Cancellation is not a rollback.

### Preserved PDF overwrite support

`remarkable_overwrite_pdf` is available in writable cloud mode and accepts an
exact document UUID and a local PDF path. It rejects deleted/trashed targets,
non-PDF targets, and changed page counts. Only PDF and metadata blobs change:
UUID, title, folder, layout, page IDs, and annotation sidecars are preserved.
SSH PDF overwrite also checks the page count. Equal page counts cannot ensure
ink alignment if PDF content moves.

This implementation includes overwrite directly. It does not depend on an
unreleased Python patch or change the stock `uvx remarkable-mcp` package.

## Build and run

Validated on Linux amd64 with Go 1.27.3, cgo, and normal build/GC defaults:

```sh
git clone https://github.com/danielmichaels/go-remarkable-mcp.git
cd go-remarkable-mcp
go build -o go-remarkable-mcp .
go test -race ./... -count=1
go vet ./...
go mod verify
```

Building requires a C compiler. Install `tesseract-ocr` to run the actual local
OCR tests; those tests skip when Tesseract is unavailable. CI runs the Linux
build, race tests, vet, module verification, and formatting checks with Tesseract.

To install from source without cloning:

```sh
go install github.com/danielmichaels/go-remarkable-mcp@latest
```

Use the binary as an MCP stdio command. Closing stdin stops it. No uv/uvx or
Python environment is needed. Optional runtime programs are `tesseract` for
local OCR, `ssh` for SSH transport, and `sshpass` for SSH password auth.
The native Story/PDF binding requires cgo and is pinned to the MuPDF 1.28.0 ABI
bundled with go-fitz v1.28.2. Non-cgo builds are not supported as replacements.

CLI supports `--local-dir [PATH]`, `--ssh`, `--ssh-key`, `--usb`, `--read-only`,
`--no-cloud-fallback`, `--register CODE`, and `--http --host HOST --port PORT`.
Default HTTP binding is loopback; this prototype has no HTTP authentication.
Do not expose it publicly without an authenticated boundary. Local mode can
also use `REMARKABLE_USE_LOCAL_DIR=1` and `REMARKABLE_LOCAL_DIR=PATH`.

Existing `REMARKABLE_*` transport/auth/read-only/root-path configuration is
supported where used by these implementations. Set `REMARKABLE_READ_ONLY=1`
for safe inspection. Deletion asks the MCP client for confirmation unless
`REMARKABLE_SKIP_CONFIRM=1`; clients without elicitation cannot confirm it.

### MCP configuration

Configure your MCP client with the absolute path to the built or installed
binary. This example uses the cloud transport in read-only mode:

```json
{
  "mcpServers": {
    "remarkable": {
      "command": "/absolute/path/to/go-remarkable-mcp",
      "args": ["--read-only"]
    }
  }
}
```

Provide cloud credentials through `REMARKABLE_TOKEN` or `~/.rmapi`, not by
committing them to this repository. To register a new device token, use
`go-remarkable-mcp --register YOUR_ONE_TIME_CODE`. Registration contacts the
reMarkable authentication service and writes `~/.rmapi`.

For SSH, add `--ssh` and configure `REMARKABLE_SSH_HOST`,
`REMARKABLE_SSH_USER`, and `REMARKABLE_SSH_KEY` as needed. For USB web access,
add `--usb`. For an offline xochitl directory, use
`--local-dir /path/to/xochitl`; this mode always stays read-only.
Remove `--read-only` only when you intend to enable library mutations.

Process start/stop and lazy loading are controlled by the MCP client. This
server does not provide an automatic ten-minute idle shutdown.

## Verification and resource profile

Tests exercise MCP workflows, schemas/defaults, root scoping, exports and
eviction, confirmation/refusal, authoring, cloud lifecycle and overwrite,
generation conflicts, ambiguous commits, caches, USB HTTP, fake SSH, and
native rendering/extraction. They make no real cloud/tablet mutations.
Google API-key and ADC paths are checked using intercepted HTTP responses,
not live Google calls. Tesseract is exercised as an actual native executable.
An optional real v6 scene fixture can be supplied with
`REMARKABLE_NATIVE_RM_FIXTURE=/absolute/path/to/page.rm`.

Opt-in differential tests use the original Python server as an external oracle,
never as a server dependency:

```sh
REMARKABLE_PYTHON_ORACLE=/path/to/reference/python go test ./... -count=1
```

The Markdown comparison checks page counts, word order, word coordinates, and
clickable link rectangles/destinations using a single external extractor.
Fixtures cover headings/lists/code, escaped HTML/images, multilingual text,
overflowing tables, pagination boundaries, styled image labels, cross-page links,
relative/named URLs, and internal Story anchors. The matching EPUB fixture checks
manifest versus spine ordering, navigation, non-spine content, text-node
boundaries, entities, cover-template text, and ebooklib-style body normalization.

A Linux amd64 stdio profile after the compatibility pass measured:

| Settled RSS | Stock Python 2.1.0 | Native Go |
|---|---:|---:|
| Small PDF read/render/export workload | 193.26 MiB | 31.88 MiB |
| EPUB + nine Markdown Story renders/uploads | 245.33 MiB | 71.23 MiB |

Both servers received the same fixture bytes and operations within each workload.
Each fresh process settled for three seconds, then idled for 60 seconds. Go's CPU
counter did not increase at 0.01-second resolution; Python added 0.01–0.02 seconds.
Neither server spawned observed child processes. Warm initialization took 9–12 ms
for Go and 1.13–1.15 seconds for Python. Multilingual Markdown was slower in Go
(318 ms versus 235 ms median), so lower memory does not mean every operation is
faster. These are short local measurements, not a large-library benchmark,
throughput guarantee, cold-start test, or 24-hour soak.

## Remaining validation limits

- Passing the differential fixtures does not establish complete Markdown,
  malformed-HTML/EPUB, font/script, or Python regex-language equivalence. Human
  hints and error prose are not compared. Pixel-identical parity is not claimed.
- Arbitrary malformed EPUB HTML can still normalize differently between the
  native and Python parsers.
- Without tablet page sidecars, `DocumentPageCount` can report EPUB reflow pages
  instead of an unknown physical count. Extraction correctly reports unknown.
  This known inconsistency still needs a fix.
- PDF archive exports replay drawing operations, retaining selectable source
  text and vectors, but do not copy interactive source annotations, outlines,
  or actions. They are exports, not byte-preserving copies of the source PDF.
- Cloud metadata/download traversal is sequential, with strict errors rather
  than Python's occasional partial results. No extraction result cache is kept.
- SSH uses a debounced FIFO refresh barrier, not Python's exact participant
  bookkeeping. Diagnostics expose aggregate native state, not full Python
  per-job tracing. Real-device wake/restart/port recovery remains untested.
- USB upload follows firmware behavior: destination parent is not honored and
  no document UUID is invented when firmware returns none.
- Native MuPDF calls cannot be interrupted mid-call; cancellation is checked
  between pages and stops OCR subprocesses. Large documents can use much more
  memory than the fixture. No physical-tablet authoring test or live Google
  OCR test has been performed.

## Distribution and licensing

The project is distributed under [AGPL-3.0](LICENSE). The Python project's MIT
notice is in `THIRD_PARTY_LICENSE.txt`; native notices
are in `render_licenses.go`. go-rmscene, fpdf, and goldmark use MIT licenses.
Bundled Droid Sans Fallback is Apache-2.0 and Noto Naskh Arabic is SIL OFL-1.1;
their full notices are in `render_fonts/` and embedded in the notice string.

**go-fitz/MuPDF requires AGPL compliance or a suitable commercial license.**
The pinned Go modules identify the dependency sources, and `LICENSE` contains the
complete AGPL text. Binary distributors must include the applicable notices and
licenses and meet corresponding-source obligations. A commercial MuPDF license
does not relicense this project's code or other dependencies. This repository
currently distributes source; it does not publish prebuilt release binaries.
