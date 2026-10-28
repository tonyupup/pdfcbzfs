package view_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonyupup/pdfcbzfs/internal/view"
)

func conf(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".pdf2cbz.conf"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "pdfidx", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExcludedPDFsStayPlainPDFs(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_000_000, 0)
	var logs []string
	c := view.NewCache(view.Config{Root: dir, Now: func() time.Time { return now },
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	skip, st1 := write(t, dir, "skip-me.pdf", pdf(3))
	keep, st2 := write(t, dir, "keep.pdf", pdf(3))
	conf(t, dir, "[ignore]\nskip-*.pdf\n")

	now = now.Add(3 * time.Second) // past the recheck interval of the (absent) file
	if c.Probe(skip, st1) != nil {
		t.Fatal("an excluded PDF must stay a PDF")
	}
	if c.Probe(keep, st2) == nil {
		t.Fatal("other PDFs are still converted")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "excluded by") || !strings.Contains(logs[0], "[ignore]: skip-*.pdf") {
		t.Fatalf("log: %q", logs)
	}

	// edit the file: the PDF is converted from then on, without a restart
	conf(t, dir, "[ignore]\nnothing-matches-*.pdf\n")
	now = now.Add(3 * time.Second)
	if c.Probe(skip, st1) == nil {
		t.Fatal("after the rule is gone the PDF must be converted")
	}
	conf(t, dir, "[ignore]\n*.pdf\n")
	now = now.Add(3 * time.Second)
	if c.Probe(skip, st1) != nil || c.Probe(keep, st2) != nil {
		t.Fatal("a new rule excludes both again")
	}
}

func TestExclusionAppliesToSubdirectoriesOnly(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "课本", "北师大")
	os.MkdirAll(sub, 0o755)
	c := view.NewCache(view.Config{Root: dir})
	inside, st1 := write(t, sub, "a.pdf", pdf(2))
	outside, st2 := write(t, dir, "b.pdf", pdf(2))
	conf(t, dir, "[ignore]\n课本/\n")
	if c.Probe(inside, st1) != nil || c.Probe(outside, st2) == nil {
		t.Fatal("only PDFs below 课本/ are excluded")
	}
	// a config file in the subdirectory re-includes what the outer one excluded?
	// No: a whole excluded directory cannot be re-included, as in git.
	conf(t, sub, "[ignore]\n!a.pdf\n")
	c2 := view.NewCache(view.Config{Root: dir})
	if c2.Probe(inside, st1) != nil {
		t.Fatal("a file below an excluded directory stays excluded")
	}
}

func TestDefaultPasswordFromTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	var logs []string
	now := time.Unix(1_000_000, 0)
	c := view.NewCache(view.Config{Root: dir, Now: func() time.Time { return now },
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	p, st := write(t, dir, "enc-aes-256.pdf", fixtureBytes(t, "enc-aes-256.pdf")) // password: user-pw
	if c.Probe(p, st) != nil {
		t.Fatal("no password known: pass through")
	}

	conf(t, dir, "[passwords]\nenc-*.pdf = wrong\nenc-*.pdf = user-pw\n*.zip = other\n")
	now = now.Add(3 * time.Second)
	if c.Probe(p, st) == nil {
		t.Fatal("a matching wildcard rule with the right password must open the PDF")
	}

	conf(t, dir, "[passwords]\nenc-*.pdf = wrong\n")
	now = now.Add(3 * time.Second)
	if c.Probe(p, st) != nil {
		t.Fatal("only wrong passwords: pass through again")
	}

	conf(t, dir, "[passwords]\nsomething-else.pdf = user-pw\n")
	now = now.Add(3 * time.Second)
	if c.Probe(p, st) != nil {
		t.Fatal("a rule for another file must not apply")
	}
	if strings.Contains(strings.Join(logs, "\n"), "user-pw") {
		t.Fatalf("a password leaked into the log: %q", logs)
	}
}

func TestPasswordRuleInAParentDirectoryAndTheFilesOwnPasswdFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "scans")
	os.MkdirAll(sub, 0o755)
	enc := fixtureBytes(t, "enc-aes-256.pdf")
	conf(t, dir, "[passwords]\nscans/ = user-pw\n")
	c := view.NewCache(view.Config{Root: dir})
	p, st := write(t, sub, "any name.pdf", enc)
	if c.Probe(p, st) == nil {
		t.Fatal("a rule for a directory above must give its PDFs the password")
	}
	// the per-file NAME.pdf.passwd keeps working, without any config
	dir2 := t.TempDir()
	c2 := view.NewCache(view.Config{Root: dir2})
	p2, st2 := write(t, dir2, "b.pdf", enc)
	if c2.Probe(p2, st2) != nil {
		t.Fatal("no password: pass through")
	}
	os.WriteFile(p2+".passwd", []byte("user-pw\n"), 0o644)
	if c2.Probe(p2, st2) == nil {
		t.Fatal(".passwd must still work")
	}
}

func TestNoRootMeansNoConfigFiles(t *testing.T) {
	dir := t.TempDir()
	conf(t, dir, "[ignore]\n*.pdf\n")
	p, st := write(t, dir, "a.pdf", pdf(2))
	if view.NewCache(view.Config{}).Probe(p, st) == nil {
		t.Fatal("without a Root the config files are not looked for")
	}
}
