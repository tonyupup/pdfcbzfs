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

Converts: DCT JPEG; Adobe CMYK JPEG only with `/Decode [1 0 1 0 1 0 1 0]` **and** an Adobe APP14 marker (unencrypted PDFs); Flate raw gray/RGB/indexed; ImageMask; CCITT G4 (`K<0`); `[Flate DCT]`; page content that draws exactly one image even if its resources list many; **image-dominant pages** (`pdfidx/layout.go`: a small content-stream interpreter tracks the CTM, clips, path boxes and text; one unrotated image covering >= 70% of the page, nothing painted over it afterwards, <= 12 visible text bytes, <= 80 paths) with their furniture dropped. Note the older rule still applies first: a page that lists exactly ONE XObject is extracted without looking at its content, and a page whose content draws only its image is extracted whatever its size.
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

Runs as a privileged sidecar next to Kavita on arm64 k3s (nodes alioth-1..4, `kubectl` works from the phone). Namespace `kavita`, Deployment `kavita` (containers `pdfcbzfs` + `kavita`, single replica). **Not managed by Flux** (Flux's only Kustomization `flux-system` tracks the Gitea repo `tony/k3s-gitops` and owns just the Ingress `kavita-traefik` of this namespace): change the Deployment with `kubectl patch`, never the Ingress. Back up first (`kubectl get deploy kavita -o yaml`), dry-run with `--dry-run=server`, `kubectl rollout undo` to go back.

- Image `ghcr.io/tonyupup/pdfcbzfs:sha-<7 chars of the master commit>` (public package, built by CI for amd64+arm64; always pin the sha tag, nodes pull through the mirror `ghcr.dockerproxy.zltop.cn` configured in every node's `/etc/rancher/k3s/registries.yaml`). The image is Alpine/musl and carries `pdfcbzfs`, `pdfcbzfs-render` and `libpdfium.so`.
- Args `/lower /cbz-out -f -v -o ro,allow_other,pages_per_cbz=200,pdf_mode=folder`; `/lower` is the NFS PVC `kavita-manga-doraemon` (ROX), `/cbz-out` an emptyDir with `mountPropagation: Bidirectional` shared with Kavita (`/manga2`, `HostToContainer`).
- Render cache on the **NFS PVC `kavita-render-cache-nfs`** (4Gi, class `nfs-client`, RWX, reclaim `Retain`) mounted at `/cache`, `PDFCBZFS_CACHE_MB=3000`. The NFS server 172.18.0.1 is the router that also holds the manga and runs aria2 (slow spinning disk): cold reads of cached pages measured ~35 ms per file (~6 MB/s), writes ~7 MB/s. The user asked for NFS, not `local-path`. Resources: requests 100m/128Mi, limits 2 CPU / 768Mi (not yet load tested in the cluster).
- Kavita's library scan reads every CBZ's central directory, which needs the CRC of every page, so a scan draws every not-yet-cached page of every book that needs rendering (CPU pegged at the 2 core limit while it runs). Reading an extracted-only volume costs nothing.
- `kavita-config` is a `local-path` RWO PVC with node affinity: the pod is pinned to alioth-2. A mutating webhook adds `TZ=Asia/Shanghai` to every container; that is not in the Deployment.
- Kavita is v0.9.1-era: libraries `教材` (/manga2/课本), `成人` (/manga2/adult), `哆啦A梦` (/manga2/general). Docs: wiki.kavitareader.com (Scanner, Metadata/Comics, Filtering). Its manga reader has no table of contents (the PDF reader does); `<SeriesGroup>` makes collections only when the library's "Manage Collections" is on.
- Needs checking on the cluster before assuming: memory under load, whether the sidecar OOMs.


## Open ideas (nothing committed to)

- Persist the page index on disk so a restart does not re-scan cold disks (a 1.4 GB JPEG PDF takes ~30 s cold).
- An `inspect` subcommand that prints per-page structure (XObjects, filters, content operators) so diagnosing a PDF does not need Python snippets.
- `GOMEMLIMIT` + native-sidecar/startupProbe manifest for the cluster.
- Considered and not worth it for now: rewriting in Rust (fuser can direct-mount and multi-thread, but the bottleneck is disk seeks), a pure Go JPEG 2000 decoder (the renderer covers it), a CSI driver (FUSE needs a mounter pod or the NFS-server approach to survive restarts).
