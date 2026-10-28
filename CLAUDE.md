# pdfcbzfs — notes for Claude

Go FUSE passthrough filesystem: shows convertible `*.pdf` as CBZ on the fly, mirrors everything else.
README.md is the user-facing description; this file is what to know before changing code.
The maintainer writes in Chinese: reply in Chinese, commit messages are Chinese (`feat:`/`fix:`/`docs:` prefix).

## Layout

- `cmd/pdfcbzfs` — libfuse-style CLI: `pdfcbzfs LOWER MOUNTPOINT [-o opts] [-f] [-d] [-v]`; daemonizes unless `-f`; heals a stale (dead) FUSE mount at start.
- `internal/pdfidx` — PDF indexer (pread only, no whole-file load): xref tables/streams, object streams, page tree, RC4/AES-128/AES-256, image classification, PNG synthesis, CCITT G4 via `x/image/ccitt`.
- `internal/view` — presentation: modes `auto|flat|folder`, part naming, `ComicInfo.xml`, probe cache (also skips PDFs with a `.aria2` control file), stable virtual inodes.
- `internal/cbz` — STORED zip writer with sizes known up front, lazy CRC32.
- `internal/fusefs` — go-fuse layer, NFS-export friendly (stable node IDs, `LookupNode`).
- `internal/testpdf` — PDF generator for tests. Extend it instead of committing big fixtures.

## Invariants (do not break)

- **Every byte count is known without decoding**: zip headers need exact sizes. JPEG pages are copied as is; raw/CCITT pages become PNGs with *stored* deflate blocks (size from geometry). Flate-wrapped JPEG is the one exception: it is inflated once at index time (budget 256 MB per PDF).
- Convertible = every page is exactly one image. Anything else is **passed through unchanged** as a `.pdf`. Never half-convert a book; the only tolerated damage is dangling page objects (skipped, limit max(3, 10%)).
- `-v` must say *why* a PDF was passed through (page object number, what the resources hold, which operators blocked it). Keep error messages that precise: the maintainer debugs from pasted logs.
- Virtual files are read-only; real files and writes pass through to LOWER.
- Do not add CGO or heavy dependencies: static arm64 binary, small memory (a router is a target). Image cache is one global byte-bounded LRU.

## What converts / what does not (learned from real files)

Converts: DCT JPEG; Adobe CMYK JPEG only with `/Decode [1 0 1 0 1 0 1 0]` **and** an Adobe APP14 marker (unencrypted PDFs); Flate raw gray/RGB/indexed; ImageMask; CCITT G4 (`K<0`); `[Flate DCT]`; page content that draws exactly one image even if its resources list many.
Does not: text/vector pages (needs a renderer), several images drawn on a page (tiles/overlays), JPEG 2000 (`JPXDecode`, incl. under Flate), CCITT G3, JBIG2, alpha/SMask, 16-bit, other `/Decode`.
Files still downloading by aria2 have a zero-filled tail (`no startxref`); that is not a bug.

## Working

- `go vet ./... && go test -race ./...`. FUSE tests skip without `/dev/fuse` or root. `cmd/pdfcbzfs` re-executes the test binary as the daemon (SIGKILLs it to test stale mounts).
- A test that cannot fail is worthless: when adding a fix, check the test fails with the fix removed (a stale-mount test once passed only because of the kernel dentry cache).
- Fixtures made by independent tools (ImageMagick `convert -compress Group4`, PIL for CMYK, pypdf for encryption) are preferred over self-made ones; see `internal/pdfidx/testdata/README.md`.
- Workflow so far: develop on the feature branch, push, merge to `master` only when asked (fast-forward). CI builds linux arm64/amd64 and refreshes the rolling `latest` pre-release on every `master` push; `v*` tags make real releases.
- Pushing tags from the cloud session returned HTTP 403; the maintainer creates `v*` tags themselves.

## Deployment context

Runs as a privileged sidecar next to Kavita on arm64 Kubernetes, FUSE mounted on a shared `emptyDir` with `mountPropagation: Bidirectional` (sidecar) / `HostToContainer` (app). Lower directory is an NFS PVC that aria2 also downloads into. Manga PDFs go to a Kavita Manga library; text PDFs stay PDFs for a Book library.

## Open ideas (nothing committed to)

- Persist the page index on disk so a restart does not re-scan cold disks (a 1.4 GB JPEG PDF takes ~30 s cold).
- An `inspect` subcommand that prints per-page structure (XObjects, filters, content operators) so diagnosing a PDF does not need Python snippets.
- `GOMEMLIMIT` + native-sidecar/startupProbe manifest for the cluster.
- Considered and not worth it for now: rewriting in Rust (fuser can direct-mount and multi-thread, but the bottleneck is disk seeks), JPEG 2000 decoding, a CSI driver (FUSE needs a mounter pod or the NFS-server approach to survive restarts).
