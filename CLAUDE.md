# pdfcbzfs — notes for Claude

Go FUSE passthrough filesystem: shows convertible `*.pdf` as CBZ on the fly, mirrors everything else.
README.md is the user-facing description; this file is what to know before changing code.
The maintainer writes in Chinese: reply in Chinese, commit messages are Chinese (`feat:`/`fix:`/`docs:` prefix).

## Layout

- `cmd/pdfcbzfs` — libfuse-style CLI: `pdfcbzfs LOWER MOUNTPOINT [-o opts] [-f] [-d] [-v]`; daemonizes unless `-f`; heals a stale (dead) FUSE mount at start.
- `internal/pdfidx` — PDF indexer (pread only, no whole-file load): xref tables/streams, object streams, page tree, RC4/AES-128/AES-256, image classification, PNG synthesis, CCITT G4 via `x/image/ccitt`.
- `internal/view` — presentation (also `config.go`: per-directory `.pdf2cbz.conf` with `[ignore]` and `[passwords]` sections, gitignore-style wildcards, inner files override outer ones, re-read every 2 s; it is part of the probe signature so edits re-probe at once; passwords never go into logs): modes `auto|flat|folder`, part naming, `ComicInfo.xml`, probe cache (also skips PDFs with a `.aria2` control file), stable virtual inodes.
- `internal/render` — draws pages through the `pdfcbzfs-render` helper: service (health + backoff, retries, singleflight, worker cap), exact-size JPEG fitting, disk cache. `helper/` holds the C helper and `build.sh` (PDFium prebuilt, pinned release).
- `internal/cbz` — STORED zip writer with sizes known up front, lazy CRC32.
- `internal/fusefs` — go-fuse layer, NFS-export friendly (stable node IDs, `LookupNode`).
- `internal/testpdf` — PDF generator for tests. Extend it instead of committing big fixtures.

## Invariants (do not break)

- **Every byte count is known without decoding**: zip headers need exact sizes. JPEG pages are copied as is; raw/CCITT pages become PNGs with *stored* deflate blocks (size from geometry). Flate-wrapped JPEG is inflated once at index time (budget 256 MB per PDF). **Drawn pages** (`internal/render`) are JPEGs whose size is a fixed budget from the pixel size (`pdfidx.renderBudget`): quality is lowered to fit, the rest is padded with COM segments.
- Convertible = every page is extractable *or* drawable. Pages that cannot be extracted become render pages only when a renderer is available (`pdfidx.Options.Render`); otherwise the PDF is **passed through unchanged**. The only tolerated damage is dangling page objects (skipped, limit max(3, 10%)); a document with skipped pages and render pages is rejected because the renderer numbers pages itself.
- The renderer is a separate process (`helper/render.c`, PDFium) so a crash or leak never touches the daemon. **No flag controls it**: availability is probed (`render.Service`, backoff 15 s to 10 min) and a PDF whose first drawn page fails stays a PDF and is probed again with backoff (`view.probeBackoff`, 30 s to 15 min). Never cache a transient failure for good.
- `-v` must say *why* a PDF was passed through (page object number, what the resources hold, which operators blocked it). Keep error messages that precise: the maintainer debugs from pasted logs.
- Virtual files are read-only; real files and writes pass through to LOWER.
- Do not add CGO or heavy Go dependencies: static arm64 binary, small memory (a router is a target). Image cache is one global byte-bounded LRU. Rendering memory: the page is kept as YCbCr 4:2:0 planes (never a full RGB copy); GC is tightened in `main.go` (GOGC 40, soft limit 192 MB).

## What converts / what does not (learned from real files)

Converts: DCT JPEG; Adobe CMYK JPEG only with `/Decode [1 0 1 0 1 0 1 0]` **and** an Adobe APP14 marker (unencrypted PDFs); Flate raw gray/RGB/indexed; ImageMask; CCITT G4 (`K<0`); `[Flate DCT]`; page content that draws exactly one image even if its resources list many.
Cannot be extracted (drawn by the renderer when installed, else the PDF stays a PDF): text/vector pages, several images drawn on a page (tiles/overlays), JPEG 2000 (`JPXDecode`, incl. under Flate), CCITT G3, JBIG2, alpha/SMask, 16-bit, other `/Decode`.
Files still downloading by aria2 have a zero-filled tail (`no startxref`); that is not a bug.

## Working

- `go vet ./... && go test -race ./...` (the phone kernel has no ThreadSanitizer: run `-race` on the WSL box). FUSE tests skip without `/dev/fuse` or root. `cmd/pdfcbzfs` re-executes the test binary as the daemon (SIGKILLs it to test stale mounts).
- A test that cannot fail is worthless: when adding a fix, check the test fails with the fix removed (a stale-mount test once passed only because of the kernel dentry cache).
- Fixtures made by independent tools (ImageMagick `convert -compress Group4`, PIL for CMYK, pypdf for encryption) are preferred over self-made ones; see `internal/pdfidx/testdata/README.md`.
- Workflow so far: develop on the feature branch, push, merge to `master` only when asked (fast-forward). CI builds linux arm64/amd64 and refreshes the rolling `latest` pre-release on every `master` push; `v*` tags make real releases.
- Pushing tags from the cloud session returned HTTP 403; the maintainer creates `v*` tags themselves.
- Heavy builds and cross compiles (the C helper for arm64, `-race`) run on the WSL x86_64 machine over ssh (it has `aarch64-linux-gnu-gcc`, `qemu-aarch64`); the phone `/` partition is nearly full: never `apt install` there, keep everything under `/home`.

## Deployment context

Runs as a privileged sidecar next to Kavita on arm64 Kubernetes (the image must be glibc based and contain `pdfcbzfs`, `pdfcbzfs-render` and `lib/libpdfium.so`; give it a cache `emptyDir` and enough memory), FUSE mounted on a shared `emptyDir` with `mountPropagation: Bidirectional` (sidecar) / `HostToContainer` (app). Lower directory is an NFS PVC that aria2 also downloads into. Manga PDFs go to a Kavita Manga library; text PDFs stay PDFs for a Book library.

## Open ideas (nothing committed to)

- Persist the page index on disk so a restart does not re-scan cold disks (a 1.4 GB JPEG PDF takes ~30 s cold).
- An `inspect` subcommand that prints per-page structure (XObjects, filters, content operators) so diagnosing a PDF does not need Python snippets.
- `GOMEMLIMIT` + native-sidecar/startupProbe manifest for the cluster.
- Considered and not worth it for now: rewriting in Rust (fuser can direct-mount and multi-thread, but the bottleneck is disk seeks), a pure Go JPEG 2000 decoder (the renderer covers it), a CSI driver (FUSE needs a mounter pod or the NFS-server approach to survive restarts).
