package render

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Request asks for one page.
type Request struct {
	Path     string
	Password []byte
	Page     int // 0-based
	DPI      float64
	MaxSide  int
}

// Backend draws pages. Check reports whether it can run at all (the helper
// starts and its libraries load).
type Backend interface {
	Render(ctx context.Context, q Request) (*Raster, error)
	Check() error
}

// Error is a failed render. Permanent errors (wrong password, not a PDF, no
// such page) are not worth retrying.
type Error struct {
	Msg       string
	Permanent bool
}

func (e *Error) Error() string { return e.Msg }

// Helper runs the pdfcbzfs-render executable, one process per page, so the
// memory PDFium needs is released as soon as the page is drawn.
type Helper struct{ Bin string }

// FindHelper looks for pdfcbzfs-render: $PDFCBZFS_RENDER_HELPER, next to the
// running executable, the user's ~/.local/pdfcbzfs/bin, /usr/local/lib and
// /usr/lib/pdfcbzfs, then PATH. libpdfium.so is expected in ../lib of the
// helper (it is linked with that RUNPATH).
func FindHelper() *Helper {
	var cands []string
	if p := os.Getenv("PDFCBZFS_RENDER_HELPER"); p != "" {
		cands = append(cands, p)
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "pdfcbzfs-render"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".local/pdfcbzfs/bin/pdfcbzfs-render"))
	}
	cands = append(cands, "/usr/local/lib/pdfcbzfs/bin/pdfcbzfs-render", "/usr/lib/pdfcbzfs/bin/pdfcbzfs-render")
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return &Helper{Bin: c}
		}
	}
	if p, err := exec.LookPath("pdfcbzfs-render"); err == nil {
		return &Helper{Bin: p}
	}
	return nil
}

// Check runs the helper without arguments: it must print its usage and exit 2,
// which proves the binary starts and PDFium loads.
func (h *Helper) Check() error {
	var stderr bytes.Buffer
	cmd := exec.Command(h.Bin)
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 2 && strings.Contains(stderr.String(), "usage") {
		return nil
	}
	return fmt.Errorf("render helper %s does not run: %v %s", h.Bin, err, strings.TrimSpace(stderr.String()))
}

// Render draws one page.
func (h *Helper) Render(ctx context.Context, q Request) (*Raster, error) {
	args := []string{"-r", strconv.FormatFloat(q.DPI, 'f', -1, 64), "-M", strconv.Itoa(q.MaxSide)}
	if len(q.Password) > 0 {
		args = append(args, "-p", string(q.Password))
	}
	args = append(args, q.Path, strconv.Itoa(q.Page))
	cmd := exec.CommandContext(ctx, h.Bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, &Error{Msg: "start render helper: " + err.Error()}
	}
	r, rerr := readPPM(bufio.NewReaderSize(out, 1<<20))
	io.Copy(io.Discard, out)
	werr := cmd.Wait()
	if werr != nil {
		msg := strings.TrimSpace(stderr.String())
		var ee *exec.ExitError
		code := -1
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		}
		if ctx.Err() != nil {
			return nil, &Error{Msg: "render timed out or was cancelled: " + ctx.Err().Error()}
		}
		// 2 usage, 3 cannot open the document, 4 cannot load the page: all
		// say something about the file, so retrying cannot help.
		return nil, &Error{Msg: fmt.Sprintf("render helper exit %d: %s", code, msg), Permanent: code == 2 || code == 3 || code == 4}
	}
	if rerr != nil {
		return nil, &Error{Msg: "bad render output: " + rerr.Error()}
	}
	return r, nil
}

// readPPM parses a binary P6 image row by row straight into planes, so a
// page never exists as a full RGB buffer.
func readPPM(br *bufio.Reader) (*Raster, error) {
	var magic string
	var w, h, max int
	if _, err := fmt.Fscan(br, &magic, &w, &h, &max); err != nil {
		return nil, err
	}
	if magic != "P6" || max != 255 || w < 1 || h < 1 || w > 20000 || h > 20000 || int64(w)*int64(h) > 200<<20 {
		return nil, fmt.Errorf("unexpected header %q %dx%d max %d", magic, w, h, max)
	}
	if _, err := br.ReadByte(); err != nil { // the single whitespace after maxval
		return nil, err
	}
	b := newRasterBuilder(w, h)
	row := make([]byte, 3*w)
	for y := 0; y < h; y++ {
		if _, err := io.ReadFull(br, row); err != nil {
			return nil, err
		}
		b.addRow(y, row)
	}
	return b.finish(), nil
}
