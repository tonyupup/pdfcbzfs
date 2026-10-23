# pdfcbzfs

A FUSE passthrough filesystem that shows every convertible `*.pdf` as CBZ
file(s), built on the fly. Everything else in the directory is mirrored
unchanged (reads **and** writes). Made so a comic server such as Kavita can
read PDF scans as proper comic archives without keeping a second copy on disk.

A PDF is convertible when every page holds exactly one JPEG (`DCTDecode`)
image, which is the usual shape of scanned books and comics. The JPEG
streams are copied out **losslessly** into an uncompressed (STORED) zip, so
there is no re-encoding and almost no CPU use. Sizes are known up front, reads
work at any offset, and CRC32s are computed lazily the first time an archive's
headers are needed.

Anything else (text PDFs, pages with several images, CMYK/`/Decode` images,
other image filters, damaged files, wrong or missing password) is **passed
through untouched** as a normal `.pdf`.

## Presentation

| PDF | `--pdf-mode auto` (default) | `--pdf-mode folder` |
| --- | --- | --- |
| `<= --pages-per-cbz` pages | `Book.cbz` next to where `Book.pdf` was | `Book/Book v01.cbz` |
| more pages | `Book/Book v01.cbz`, `v02.cbz`, ... | same |

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

```
pdfcbzfs --lower DIR [--pages-per-cbz 200] [--pdf-mode auto|folder] [--ro] [--allow-other=true] [-v] MOUNTPOINT
```

It mounts with `mount(2)` directly (no `fusermount` needed) and must run as
root for that; with a `fusermount` available it also works unprivileged.

To replace a directory in place (the usual deployment), keep a second view of
the real directory and mount over the original path:

```sh
mkdir -p /srv/manga-real
mount --bind /srv/manga /srv/manga-real
pdfcbzfs --lower /srv/manga-real /srv/manga &
```

Unmounting (`umount /srv/manga`) brings the original directory back; nothing
on disk is ever modified by the conversion.

## Exporting over NFS

The filesystem advertises `FUSE_EXPORT_SUPPORT`, uses stable node IDs (the
lower inode for real files, a hash for virtual ones) and implements
`LookupNode`, so knfsd can export it. Notes:

* A FUSE mount nested under another export is **not** visible to clients
  unless it has its own entry in `/etc/exports` with an explicit `fsid=`.
* After the daemon restarts, NFS clients may see `ESTALE` for handles they
  already held until they re-lookup; remounting the client is the reliable fix.
  The daemon re-resolves handles by walking the tree, which is slow on very
  large trees.
* Virtual node IDs change when the PDF's size or mtime changes (the
  generation number changes too), as NFS requires.

## Limitations

* Pages are not re-rendered: no text PDFs, no layered pages.
* A part larger than 4 GiB or with more than 65535 images (ZIP64) is not
  generated; use a smaller `--pages-per-cbz`.
* Chapter boundaries are not recoverable from a PDF; parts are fixed-size
  slices of `--pages-per-cbz` pages.
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
