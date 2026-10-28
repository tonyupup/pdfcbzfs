/*
 * pdfcbzfs-render: render one PDF page to a binary PPM (P6) on stdout using
 * PDFium. A separate process so that a crash or a leak in the renderer never
 * touches the filesystem daemon, and so the memory it uses goes away when it
 * exits.
 *
 *   pdfcbzfs-render [-p PASSWORD] [-r DPI] [-M MAXSIDE] FILE PAGE
 *
 * PAGE is 0-based. The page is drawn on white at DPI (default 150), reduced if
 * needed so that neither side exceeds MAXSIDE pixels (default 3000).
 * Exit status: 0 ok, 2 usage, 3 cannot open document (PDFium error on stderr),
 * 4 cannot load/render page, 5 out of memory.
 *
 * Build: cc -O2 -o pdfcbzfs-render render.c -Iinclude -Llib -lpdfium
 */
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "fpdfview.h"

static int usage(void) {
	fprintf(stderr, "usage: pdfcbzfs-render [-p PASSWORD] [-r DPI] [-M MAXSIDE] FILE PAGE\n");
	return 2;
}

int main(int argc, char **argv) {
	const char *password = NULL;
	double dpi = 150;
	int maxside = 3000;
	int i = 1;
	for (; i < argc && argv[i][0] == '-' && argv[i][1]; i++) {
		if (!strcmp(argv[i], "-p") && i + 1 < argc) {
			password = argv[++i];
		} else if (!strcmp(argv[i], "-r") && i + 1 < argc) {
			dpi = atof(argv[++i]);
		} else if (!strcmp(argv[i], "-M") && i + 1 < argc) {
			maxside = atoi(argv[++i]);
		} else {
			return usage();
		}
	}
	if (argc - i != 2 || dpi < 10 || dpi > 600 || maxside < 64 || maxside > 16000) {
		return usage();
	}
	const char *file = argv[i];
	int index = atoi(argv[i + 1]);
	if (index < 0) {
		return usage();
	}

	FPDF_LIBRARY_CONFIG cfg;
	memset(&cfg, 0, sizeof cfg);
	cfg.version = 2;
	FPDF_InitLibraryWithConfig(&cfg);

	int rc = 0;
	FPDF_DOCUMENT doc = FPDF_LoadDocument(file, password);
	if (!doc) {
		fprintf(stderr, "cannot open %s: pdfium error %lu\n", file, FPDF_GetLastError());
		rc = 3;
		goto out;
	}
	FPDF_PAGE page = FPDF_LoadPage(doc, index);
	if (!page) {
		fprintf(stderr, "cannot load page %d (document has %d)\n", index, FPDF_GetPageCount(doc));
		rc = 4;
		goto closedoc;
	}

	double wpt = FPDF_GetPageWidth(page), hpt = FPDF_GetPageHeight(page);
	double scale = dpi / 72.0;
	double longest = (wpt > hpt ? wpt : hpt) * scale;
	if (longest > maxside) {
		scale *= maxside / longest;
	}
	int w = (int)lround(wpt * scale), h = (int)lround(hpt * scale);
	if (w < 1) w = 1;
	if (h < 1) h = 1;

	FPDF_BITMAP bmp = FPDFBitmap_Create(w, h, 0); /* BGRx */
	if (!bmp) {
		fprintf(stderr, "cannot allocate %dx%d bitmap\n", w, h);
		rc = 5;
		goto closepage;
	}
	FPDFBitmap_FillRect(bmp, 0, 0, w, h, 0xFFFFFFFF);
	FPDF_RenderPageBitmap(bmp, page, 0, 0, w, h, 0, FPDF_ANNOT);

	const unsigned char *px = (const unsigned char *)FPDFBitmap_GetBuffer(bmp);
	int stride = FPDFBitmap_GetStride(bmp);
	unsigned char *row = malloc((size_t)w * 3);
	if (!px || !row) {
		rc = 5;
		free(row);
		goto closebmp;
	}
	printf("P6\n%d %d\n255\n", w, h);
	for (int y = 0; y < h; y++) {
		const unsigned char *s = px + (size_t)y * stride;
		for (int x = 0; x < w; x++) {
			row[3 * x] = s[4 * x + 2];
			row[3 * x + 1] = s[4 * x + 1];
			row[3 * x + 2] = s[4 * x];
		}
		if (fwrite(row, 3, w, stdout) != (size_t)w) {
			rc = 4; /* reader went away */
			break;
		}
	}
	free(row);
	if (fflush(stdout) != 0 && rc == 0) {
		rc = 4;
	}
closebmp:
	FPDFBitmap_Destroy(bmp);
closepage:
	FPDF_ClosePage(page);
closedoc:
	FPDF_CloseDocument(doc);
out:
	FPDF_DestroyLibrary();
	return rc;
}
