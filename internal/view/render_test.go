package view_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tonyupup/pdfcbzfs/internal/render"
	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
	"github.com/tonyupup/pdfcbzfs/internal/view"
)

type fakeRenderer struct {
	mu        sync.Mutex
	available bool
	err       error
	calls     []render.PageKey
}

func (f *fakeRenderer) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

func (f *fakeRenderer) Page(_ context.Context, k render.PageKey) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, k)
	if f.err != nil {
		return nil, f.err
	}
	return bytes.Repeat([]byte{byte(k.Page + 1)}, int(k.Budget)), nil
}

func (f *fakeRenderer) set(available bool, err error) {
	f.mu.Lock()
	f.available, f.err = available, err
	f.mu.Unlock()
}

// textish has a page (the 2nd of 4) that cannot be extracted: it draws two images.
func textish() []byte {
	var j [][]byte
	for i := 0; i < 4; i++ {
		j = append(j, testpdf.JPEG(i, 16, 16))
	}
	return testpdf.Classic(j, testpdf.Opts{ExtraImage: 2})
}

func TestPDFWithUnextractablePageIsConvertedWhenARendererIsAvailable(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRenderer{available: true}
	var logs []string
	c := view.NewCache(view.Config{Render: fr, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	p, st := write(t, dir, "book.pdf", textish())
	v := c.Probe(p, st)
	if v == nil || len(v.Parts) != 1 {
		t.Fatalf("must be converted: %+v", v)
	}
	// the probe drew the first render page, and only that
	if len(fr.calls) != 1 || fr.calls[0].Page != 1 || fr.calls[0].Path != p || fr.calls[0].DPI != 150 || fr.calls[0].MaxSide != 2400 {
		t.Fatalf("probe calls: %+v", fr.calls)
	}
	if fr.calls[0].FileID == "" || fr.calls[0].Budget == 0 {
		t.Fatalf("incomplete key %+v", fr.calls[0])
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "1 of 4 pages are drawn by the renderer") {
		t.Fatalf("logs: %q", logs)
	}
	// reading the whole archive goes through the renderer for that page only
	f, _ := os.Open(p)
	defer f.Close()
	all := make([]byte, v.Parts[0].Size())
	if n, err := v.Parts[0].ReadAt(f, all, 0); n != len(all) {
		t.Fatalf("read %d of %d: %v", n, len(all), err)
	}
	if !bytes.Contains(all, bytes.Repeat([]byte{2}, 1000)) {
		t.Fatal("the rendered page bytes are not in the archive")
	}
}

func TestWithoutARendererTheSamePDFIsPassedThrough(t *testing.T) {
	dir := t.TempDir()
	p, st := write(t, dir, "book.pdf", textish())
	if v := view.NewCache(view.Config{}).Probe(p, st); v != nil {
		t.Fatal("no renderer configured: pass through")
	}
	fr := &fakeRenderer{available: false}
	if v := view.NewCache(view.Config{Render: fr}).Probe(p, st); v != nil || len(fr.calls) != 0 {
		t.Fatal("renderer not available: pass through without calling it")
	}
}

func TestPDFsThatNeedNoRenderingNeverCallTheRenderer(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRenderer{available: true}
	p, st := write(t, dir, "plain.pdf", pdf(3))
	if v := view.NewCache(view.Config{Render: fr}).Probe(p, st); v == nil || len(fr.calls) != 0 {
		t.Fatalf("calls %v", fr.calls)
	}
}

func TestFailedProbeIsRetriedWithBackoff(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRenderer{available: true, err: errors.New("helper killed")}
	now := time.Unix(1_000_000, 0)
	var logs []string
	c := view.NewCache(view.Config{Render: fr, Now: func() time.Time { return now },
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	p, st := write(t, dir, "book.pdf", textish())

	if c.Probe(p, st) != nil {
		t.Fatal("canary failed: pass through")
	}
	if len(fr.calls) != 1 {
		t.Fatalf("%d calls", len(fr.calls))
	}
	if !strings.Contains(logs[len(logs)-1], "trying again in 30s") {
		t.Fatalf("log: %q", logs[len(logs)-1])
	}
	for i := 0; i < 5; i++ { // inside the backoff window nothing is tried again
		now = now.Add(5 * time.Second)
		c.Probe(p, st)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("re-probed inside the backoff window: %d calls", len(fr.calls))
	}
	now = now.Add(10 * time.Second) // 35s after the failure
	if c.Probe(p, st) != nil || len(fr.calls) != 2 {
		t.Fatalf("calls %d", len(fr.calls))
	}
	if !strings.Contains(logs[len(logs)-1], "trying again in 1m0s") {
		t.Fatalf("second failure must back off longer: %q", logs[len(logs)-1])
	}
	now = now.Add(61 * time.Second)
	fr.set(true, nil) // the renderer recovers
	v := c.Probe(p, st)
	if v == nil {
		t.Fatal("must be converted after the retry succeeds")
	}
	if c.Probe(p, st) != v {
		t.Fatal("a successful probe is remembered, not repeated")
	}
}

func TestBackoffIsCapped(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRenderer{available: true, err: errors.New("down")}
	now := time.Unix(1_000_000, 0)
	var logs []string
	c := view.NewCache(view.Config{Render: fr, Now: func() time.Time { return now },
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	p, st := write(t, dir, "book.pdf", textish())
	for i := 0; i < 12; i++ {
		c.Probe(p, st)
		now = now.Add(time.Hour)
	}
	if last := logs[len(logs)-1]; !strings.Contains(last, "trying again in 15m0s") {
		t.Fatalf("cap: %q", last)
	}
}

func TestRendererBecomingAvailableReprobesAtOnce(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRenderer{available: false}
	c := view.NewCache(view.Config{Render: fr})
	p, st := write(t, dir, "book.pdf", textish())
	if c.Probe(p, st) != nil {
		t.Fatal("pass through while unavailable")
	}
	fr.set(true, nil) // the helper got installed
	if c.Probe(p, st) == nil {
		t.Fatal("must be converted as soon as the renderer is available")
	}
	fr.set(false, nil) // and breaks again: back to the plain PDF
	if c.Probe(p, st) != nil {
		t.Fatal("must fall back to pass through when the renderer goes away")
	}
}

func TestUnencryptedPDFPassesNoPasswordToTheRenderer(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRenderer{available: true}
	c := view.NewCache(view.Config{Render: fr})
	p, st := write(t, dir, "book.pdf", textish())
	if v := c.Probe(p, st); v == nil {
		t.Fatal("not converted")
	}
	if len(fr.calls[0].Password) != 0 {
		t.Fatalf("an unencrypted PDF must not pass a password: %q", fr.calls[0].Password)
	}
}

// A page that is an image with a frame and a page number around it needs no
// renderer: its image is extracted, so a book made of such pages costs no CPU.
func TestImageDominantPagesNeverCallTheRenderer(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRenderer{available: true}
	var j [][]byte
	for i := 0; i < 4; i++ {
		j = append(j, testpdf.JPEG(i, 16, 16))
	}
	content := "1 g 0 0 200 300 re f 0.8 g 0 0 200 8 re f 0.8 g 0 292 200 8 re f q 180 0 0 270 10 20 cm /Im0 Do Q BT /F1 9 Tf 95 3 Td (7) Tj ET\n"
	pdfBytes := testpdf.Classic(j, testpdf.Opts{PageAttrs: "/MediaBox [0 0 200 300]", UnusedImage: 1,
		Content: map[int]string{1: content, 2: content, 3: content, 4: content}})
	p, st := write(t, dir, "comic.pdf", pdfBytes)
	// page 1 lists a second XObject, so it is judged by the content (new rule);
	// if that rule did not hold, the probe would draw page 1 and calls would not be 0
	v := view.NewCache(view.Config{Render: fr}).Probe(p, st)
	if v == nil {
		t.Fatal("must be converted")
	}
	if len(fr.calls) != 0 {
		t.Fatalf("the renderer was called %d times for pages that are plain images", len(fr.calls))
	}
	f, _ := os.Open(p)
	defer f.Close()
	all := make([]byte, v.Parts[0].Size())
	if n, err := v.Parts[0].ReadAt(f, all, 0); n != len(all) {
		t.Fatalf("read %d of %d: %v", n, len(all), err)
	}
	for i, img := range j {
		if !bytes.Contains(all, img) {
			t.Fatalf("page %d image is not in the archive as it is", i+1)
		}
	}
}
