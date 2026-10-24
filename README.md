# pdfcbzfs

A FUSE passthrough filesystem that shows every convertible `*.pdf` as CBZ
file(s), built on the fly. Everything else in the directory is mirrored
unchanged (reads **and** writes). Made so a comic server such as Kavita can
read PDF scans as proper comic archives without keeping a second copy on disk.

A PDF is convertible when every page holds exactly one image, which is the
usual shape of scanned books and comics:

* **JPEG** (`DCTDecode`) pages are copied out byte for byte (`0001.jpg`, ...):
  no re-encoding and almost no CPU use.
* **Flate raw-sample** pages (1/2/4/8-bit gray, 8-bit RGB, or Indexed palettes
  over those; ICCBased with N=1/3; also 1-bit `/ImageMask` stencils with
  `/Decode [0 1]` or `[1 0]`, drawn as black ink on a white page; also
  1-bit **CCITT Group 4** (`CCITTFaxDecode`, `K < 0`) scans, decoded on read)
  are wrapped losslessly into a PNG
  (`0001.png`, ...) with the original bit depth and palette. The PNG uses
  stored (uncompressed) deflate blocks, so its size is known exactly from the
  image geometry without decoding anything, and pages are only decoded when
  read.

The archive is an uncompressed (STORED) zip. Sizes are known up front, reads
work at any offset, and CRC32s are computed lazily the first time an archive's
headers are needed.

Anything else (text/layered PDFs, pages with several images, CMYK or `/Decode`
images, alpha/`/SMask`, JPEG 2000, 16-bit, CCITT Group 3 (`K >= 0`), JBIG2 and
other filters, damaged files, wrong
or missing password) is **passed through untouched** as a normal `.pdf`. Run
with `-v` to see why a given PDF was passed through.

A page may list extra XObjects (unused forms, stamps) as long as exactly one is
an image and the page content only places that image (graphics state, colour
and marked-content operators and one `Do`); a visible form, text, path or
shading keeps the PDF unconverted. `-v` names the page object and what it holds.

A few damaged pages are tolerated: if a page object is missing from the file
or its xref entry points at the wrong place (for example an ad page cut out of
the file), that page is left out of the CBZ and `-v` logs the object numbers.
More than 3 pages (or 10% of the book) missing counts as a damaged file and
the PDF is passed through. Junk before the `%PDF-` header and large padding
after `%%EOF` are also accepted.

## Presentation

Chosen with `-o pdf_mode=...`:

| PDF | `auto` (default) | `flat` | `folder` |
| --- | --- | --- | --- |
| `<= pages_per_cbz` pages | `Book.cbz` next to where `Book.pdf` was | `Book.cbz` | `Book/Book v01.cbz` |
| more pages | `Book/Book v01.cbz`, `v02.cbz`, ... | `Book v01.cbz`, `Book v02.cbz`, ... in the same directory | `Book/Book v01.cbz`, `v02.cbz`, ... |

`flat` never creates subdirectories: every CBZ sits directly beside the
(hidden) PDF. `folder` always puts the parts in a subdirectory named after the
PDF.

The original `Book.pdf` is hidden. Multi-part archives carry a
`ComicInfo.xml` with `<Series>` and `<Number>`. If a real file or directory
already has the virtual name, the real one wins.

## Password-protected PDFs

RC4 (40/128-bit), AES-128 and AES-256 (R5/R6) are supported. A PDF protected
by an empty user password opens with no setup. Otherwise put the password in
`Book.pdf.passwd` next to it (first line; additional lines are extra
candidates). Both user and owner passwords work. Changing the `.passwd` file
re-probes the PDF. The `.passwd` file stays visible in the mount.

## Usage

Standard FUSE command line:

```
pdfcbzfs LOWER MOUNTPOINT [-o opt[,opt...]] [-f] [-d] [-v]
```

| Option | Meaning |
| --- | --- |
| `-o ro` / `-o rw` | read-only / read-write (default rw) |
| `-o allow_other` | let other users access the mount (needed for knfsd or a media server running as another user) |
| `-o pages_per_cbz=N` | pages per generated CBZ (default 200) |
| `-o pdf_mode=auto\|flat\|folder` | see Presentation (default `auto`) |
| `-f` | stay in the foreground (default: daemonize once the mount succeeded; mount errors go to stderr and the exit status) |
| `-d` | log FUSE requests (implies `-f`) |
| `-v` | log why PDFs are passed through (use with `-f`) |

Usual mount(8) options (`nosuid`, `nodev`, `noatime`, `default_permissions`,
`x-*`, ...) are accepted and ignored.

It mounts with `mount(2)` directly (no `fusermount` needed) and must run as
root for that; with a `fusermount` available it also works unprivileged.

If a previous instance was killed without unmounting (OOM, `kill -9`, crash),
the dead FUSE mount it left at MOUNTPOINT (`Transport endpoint is not
connected`) is detached automatically on the next start, so a restarted
sidecar recovers by itself.

As a mount helper (needs `mount.fuse`/`mount.fuse3` from the fuse package, and
`pdfcbzfs` in `PATH`):

```sh
mount -t fuse.pdfcbzfs -o ro,allow_other,pages_per_cbz=200 /srv/manga-real /srv/manga
# /etc/fstab
/srv/manga-real  /srv/manga  fuse.pdfcbzfs  ro,allow_other,pages_per_cbz=200,nofail  0 0
```

To replace a directory in place (the usual deployment), keep a second view of
the real directory and mount over the original path:

```sh
mkdir -p /srv/manga-real
mount --bind /srv/manga /srv/manga-real
pdfcbzfs /srv/manga-real /srv/manga -o allow_other
```

Unmounting (`umount /srv/manga`) brings the original directory back; nothing
on disk is ever modified by the conversion.

## Exporting over NFS

The filesystem advertises `FUSE_EXPORT_SUPPORT`, uses stable node IDs (the
lower inode for real files, a hash for virtual ones) and implements
`LookupNode`, so knfsd can export it. Notes:

* A FUSE mount nested under another export is **not** visible to clients
  unless it has its own entry in `/etc/exports` with an explicit `fsid=`.
* Daemon restart: tested with the in-kernel NFS server (NFSv3 client holding
  an open file). After `kill -9` of the daemon, un-exporting, starting a new
  daemon on the same mount point and exporting it again with the **same
  `fsid`**, reads through the old file handle succeeded. The new daemon
  received `LOOKUP <nodeid> "."` for both a virtual CBZ and a real directory
  and resolved them through `LookupNode`. The export must be re-established
  after the new mount appears (`exportfs -r`, or un-export and export); leaving
  the old export in place was not tested. `LookupNode` finds an ID by walking
  the lower tree (and indexing the PDFs on the way), which is slow on very
  large trees.
* Virtual node IDs change when the PDF's size or mtime changes (the
  generation number changes too), as NFS requires.

## Measured

aarch64 router (4 cores, ~900 MB free RAM), spinning/slow disk, Go 1.27 static
binary:

| Case | Result |
| --- | --- |
| 1.4 GB, 4141-page JPEG PDF, 200 pages/part | 21 parts of 51-89 MB. Peak RSS 12 MB after indexing, 27 MB after reading all 1.4 GB. |
| Cold first listing of that PDF | ~30 s: ~8300 random 4 KB reads (page and image dictionaries sit between the image streams) at ~4 ms each. Warm: instant. Disabling readahead and reading in parallel did not help on this disk. |
| First open of a part (CRC pass) | ~1 s for 200 JPEGs |
| 36 MB, 189-page 4-bit indexed Flate PDF | one 91 MB CBZ of PNGs; ~6 s for the first open (decode + CRC of every page), then cached while the process lives |
| Correctness | every JPEG of part 1 and every PNG's rows + palette matched pypdf's extraction (sha256) |

Indexes and CRCs live in memory only, so each process start pays the cold
indexing cost once per PDF.

## Limitations

* Pages are not re-rendered: no text PDFs, no layered pages. PNG pages are not
  compressed (a 4-bit scan becomes roughly 2x the size of the PDF's Flate
  stream); that is the price of exact sizes without decoding.
* CCITT G4 pages shrink extremely well in the PDF but the PNG is stored
  uncompressed (a 300 dpi A4 page of ~30 KB becomes ~1 MB), so such CBZs are
  much larger than the PDF.
* ICC profiles are not carried into the PNG.
* A part larger than 4 GiB or with more than 65535 images (ZIP64) is not
  generated; use a smaller `pages_per_cbz`.
* Chapter boundaries are not recoverable from a PDF; parts are fixed-size
  slices of `pages_per_cbz` pages.
* Virtual files are read-only.

## Build

```
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o pdfcbzfs ./cmd/pdfcbzfs
```

GitHub Actions builds static `linux/arm64` and `linux/amd64` binaries on every
push (artifact) and attaches them to releases for tags `v*`.

## Tests

`go test ./...`. The FUSE integration tests skip themselves when `/dev/fuse`
or a mount is unavailable. To try a real file:
`PDFCBZFS_TEST_PDF=/path/book.pdf PDFCBZFS_TEST_PASSWORD=secret go test ./internal/pdfidx -run RealPDF -v`.
Encrypted fixtures in `internal/pdfidx/testdata` were produced by pypdf, an
independent implementation.
