# syntax=docker/dockerfile:1
#
# pdfcbzfs on Alpine: the filesystem daemon plus the PDF renderer (PDFium, the
# musl build from bblanchon/pdfium-binaries, pinned). Multi-arch: amd64, arm64.
#
#   docker buildx build --platform linux/arm64 -t pdfcbzfs .
#
# Run it as a privileged sidecar (it mounts a FUSE filesystem):
#   pdfcbzfs LOWER MOUNTPOINT -f -o allow_other,...
ARG ALPINE=3.22
ARG PDFIUM_RELEASE=chromium/8125

# The Go build runs on the build machine and cross-compiles: no emulation.
FROM --platform=$BUILDPLATFORM golang:alpine AS gobuild
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/pdfcbzfs ./cmd/pdfcbzfs

# The helper is C against musl, so it is built for the target platform.
FROM alpine:${ALPINE} AS helper
ARG PDFIUM_RELEASE
ARG TARGETARCH
RUN apk add --no-cache build-base curl
COPY helper/render.c /src/render.c
RUN set -eux; \
    case "$TARGETARCH" in arm64) p=arm64 ;; amd64) p=x64 ;; *) echo "unsupported architecture $TARGETARCH" >&2; exit 1 ;; esac; \
    mkdir /pdfium /out /out/bin /out/lib /out/licenses; \
    curl -fsSL "https://github.com/bblanchon/pdfium-binaries/releases/download/${PDFIUM_RELEASE}/pdfium-linux-musl-${p}.tgz" | tar xz -C /pdfium; \
    gcc -O2 -Wall -Wextra -o /out/bin/pdfcbzfs-render /src/render.c \
        -I/pdfium/include -L/pdfium/lib -lpdfium -lm -Wl,-rpath,'$ORIGIN/../lib'; \
    strip /out/bin/pdfcbzfs-render; \
    cp /pdfium/lib/libpdfium.so /out/lib/; \
    cp -r /pdfium/licenses/. /out/licenses/; \
    cp /pdfium/LICENSE /out/licenses/PDFium-LICENSE

FROM alpine:${ALPINE}
# fuse3 only provides fusermount3, the fallback when the container may not call
# mount(2) itself.
RUN apk add --no-cache fuse3
COPY --from=gobuild /out/pdfcbzfs /usr/local/lib/pdfcbzfs/bin/pdfcbzfs
COPY --from=helper /out/ /usr/local/lib/pdfcbzfs/
# pdfcbzfs looks for the helper next to its own (resolved) executable.
RUN ln -s /usr/local/lib/pdfcbzfs/bin/pdfcbzfs /usr/local/bin/pdfcbzfs \
 && ln -s /usr/local/lib/pdfcbzfs/bin/pdfcbzfs-render /usr/local/bin/pdfcbzfs-render \
 && pdfcbzfs-render 2>&1 | grep -q usage   # fail the build if PDFium does not load here
ENV PDFCBZFS_CACHE=/cache
ENTRYPOINT ["/usr/local/bin/pdfcbzfs"]
