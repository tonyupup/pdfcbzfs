package view

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLog) logf(f string, a ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}

func (l *testLog) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func writeConf(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newCfg(root string, log *testLog) *dirConfig {
	return newDirConfig(root, time.Now, log.logf)
}

func TestIgnorePatterns(t *testing.T) {
	for _, c := range []struct {
		name string
		conf string
		want map[string]bool // path below the root -> excluded
	}{
		{"star", "[ignore]\n*.pdf", map[string]bool{"a.pdf": true, "d/x.pdf": true, "d/e/y.PDF": true, "a.txt.bak": false}},
		{"name at any depth", "[ignore]\ndraft*", map[string]bool{"draft1.pdf": true, "d/draft.pdf": true, "mydraft.pdf": false}},
		{"anchored by a leading slash", "[ignore]\n/top.pdf", map[string]bool{"top.pdf": true, "d/top.pdf": false}},
		{"anchored by a slash inside", "[ignore]\nsub/*.pdf", map[string]bool{"sub/a.pdf": true, "x/sub/a.pdf": false, "sub/deep/a.pdf": false}},
		{"double star in the middle", "[ignore]\n**/old/*.pdf", map[string]bool{"old/a.pdf": true, "x/y/old/a.pdf": true, "old/z/a.pdf": false}},
		{"double star at the end", "[ignore]\nold/**", map[string]bool{"old/a.pdf": true, "old/z/a.pdf": true, "x/old/a.pdf": false}},
		{"directory only", "[ignore]\n课本/", map[string]bool{"课本/a.pdf": true, "x/课本/a.pdf": true, "课本.pdf": false}},
		{"directory only never matches a file of that name", "[ignore]\nbook.pdf/", map[string]bool{"book.pdf": false, "d/book.pdf": false, "book.pdf/x.pdf": true}},
		{"negation", "[ignore]\n*.pdf\n!keep.pdf", map[string]bool{"keep.pdf": false, "other.pdf": true, "d/keep.pdf": false}},
		{"a file cannot be re-included below an excluded directory", "[ignore]\nold/\n!old/a.pdf", map[string]bool{"old/a.pdf": true}},
		{"character class and question mark", "[ignore]\n[0-9]*.pdf\nv?.pdf", map[string]bool{"1book.pdf": true, "book.pdf": false, "v1.pdf": true, "v12.pdf": false}},
		{"comments blanks CRLF and trailing spaces", "[ignore]\r\n# *.pdf\r\n\r\n*.tmp.pdf   \r\n", map[string]bool{"a.pdf": false, "a.tmp.pdf": true}},
		{"escaped hash and bang", "[ignore]\n\\#weird.pdf\n\\!bang.pdf", map[string]bool{"#weird.pdf": true, "!bang.pdf": true, "weird.pdf": false}},
		{"case insensitive", "[ignore]\n*.PDF\nBOOK.pdf", map[string]bool{"x.pdf": true, "d/book.PDF": true}},
		{"byte order mark", "\xef\xbb\xbf[ignore]\n*.pdf", map[string]bool{"a.pdf": true}},
		{"section name is case insensitive", "[IGNORE]\n*.pdf", map[string]bool{"a.pdf": true}},
		{"a bracket pattern is not a section", "[ignore]\n[abc]*.pdf", map[string]bool{"a1.pdf": true, "d1.pdf": false}},
		{"spaces and CJK in names", "[ignore]\n北师大* 第?册.pdf", map[string]bool{"北师大数学 第3册.pdf": true, "北师大数学 第10册.pdf": false}},
		{"no section no effect", "*.pdf", map[string]bool{"a.pdf": false}},
		{"unknown section ignored", "[other]\n*.pdf", map[string]bool{"a.pdf": false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeConf(t, root, c.conf)
			cfg := newCfg(root, &testLog{})
			for rel, want := range c.want {
				got, why, _ := cfg.lookup(filepath.Join(root, filepath.FromSlash(rel)))
				if got != want {
					t.Errorf("%q: excluded = %v (%s), want %v", rel, got, why, want)
				}
			}
		})
	}
}

func TestIgnoreReasonNamesFileSectionAndLine(t *testing.T) {
	root := t.TempDir()
	writeConf(t, root, "[ignore]\n*.draft.pdf")
	_, why, _ := newCfg(root, &testLog{}).lookup(filepath.Join(root, "a.draft.pdf"))
	if !strings.Contains(why, configFileName) || !strings.Contains(why, "[ignore]") || !strings.Contains(why, "*.draft.pdf") {
		t.Fatalf("why = %q", why)
	}
}

func TestConfigFilesInAnyDirectoryOverrideOuterOnes(t *testing.T) {
	root := t.TempDir()
	writeConf(t, root, "[ignore]\n*.pdf")
	writeConf(t, filepath.Join(root, "d"), "[ignore]\n!keep.pdf\n/only-here.pdf")
	cfg := newCfg(root, &testLog{})
	for rel, want := range map[string]bool{
		"keep.pdf":            true,  // the root rule still applies at the root
		"d/keep.pdf":          false, // re-included by the inner file
		"d/other.pdf":         true,
		"d/e/keep.pdf":        false, // the inner file covers everything below it
		"d/only-here.pdf":     true,
		"d/e/only-here.pdf":   true, // excluded by the root *.pdf anyway
		"elsewhere/keep.pdf":  true,
		"d/e/f/g/keep.pdf":    false,
		"other/d/keep.pdf":    true, // a different "d": no inner file there
		"d/e/only-here.pdf.x": false,
	} {
		if got, why, _ := cfg.lookup(filepath.Join(root, rel)); got != want {
			t.Errorf("%q: excluded = %v (%s), want %v", rel, got, why, want)
		}
	}
	// patterns of an inner file are relative to its own directory
	root2 := t.TempDir()
	writeConf(t, filepath.Join(root2, "d"), "[ignore]\n/a.pdf")
	cfg2 := newCfg(root2, &testLog{})
	for rel, want := range map[string]bool{"d/a.pdf": true, "a.pdf": false, "d/e/a.pdf": false} {
		if got, _, _ := cfg2.lookup(filepath.Join(root2, rel)); got != want {
			t.Errorf("%q: excluded = %v, want %v", rel, got, want)
		}
	}
}

func TestBadLinesAreReportedAndSkipped(t *testing.T) {
	root := t.TempDir()
	log := &testLog{}
	writeConf(t, root, "stray line before any section\n[ignore]\n[\n*.skip.pdf\n[passwords]\nno equals sign here\ns3cret-only\n*.pdf = \n!x.pdf = pw\n\"unterminated = pw\n")
	cfg := newCfg(root, log)
	if got, _, _ := cfg.lookup(filepath.Join(root, "a.skip.pdf")); !got {
		t.Fatal("the good line must still apply")
	}
	out := log.joined()
	for _, want := range []string{"line 1", "outside a known section", "line 3", "bad pattern", "line 6", `expected "pattern = password"`, "empty password", `"!" has no meaning`, "unterminated quote"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, " pw") {
		t.Errorf("a password leaked into the log:\n%s", out)
	}
}

func TestPasswordRules(t *testing.T) {
	root := t.TempDir()
	writeConf(t, root, strings.Join([]string{
		"[passwords]",
		"# comment",
		"*.pdf = fallback",
		"课本/*.pdf = 123456",
		"x.pdf = a=b",
		"y.pdf = my pass word",
		`"a=b*.pdf" = "  secret  "`,
		"Z.PDF = upper",
		"课本/ = whole-dir",
		"dup.pdf = fallback",
	}, "\n"))
	cfg := newCfg(root, &testLog{})
	pws := func(rel string) string {
		_, _, p := cfg.lookup(filepath.Join(root, filepath.FromSlash(rel)))
		var s []string
		for _, b := range p {
			s = append(s, string(b))
		}
		return strings.Join(s, "|")
	}
	for rel, want := range map[string]string{
		"book.pdf":     "fallback",
		"课本/a.pdf":     "whole-dir|123456|fallback", // later lines first, each once
		"x.pdf":        "a=b|fallback",              // split at the first "="
		"y.pdf":        "my pass word|fallback",
		"a=b1.pdf":     "  secret  |fallback", // quotes keep the spaces
		"d/z.pdf":      "upper|fallback",      // case-insensitive
		"dup.pdf":      "fallback",            // no duplicates
		"课本/sub/b.txt": "whole-dir",           // the directory rule covers everything below
		"readme":       "",
	} {
		if got := pws(rel); got != want {
			t.Errorf("%q: passwords %q, want %q", rel, got, want)
		}
	}
}

func TestInnerPasswordsComeFirst(t *testing.T) {
	root := t.TempDir()
	writeConf(t, root, "[passwords]\n*.pdf = outer")
	writeConf(t, filepath.Join(root, "d"), "[passwords]\n*.pdf = inner\n/special.pdf = special")
	cfg := newCfg(root, &testLog{})
	_, _, p := cfg.lookup(filepath.Join(root, "d", "special.pdf"))
	var got []string
	for _, b := range p {
		got = append(got, string(b))
	}
	if strings.Join(got, "|") != "special|inner|outer" {
		t.Fatalf("passwords %v", got)
	}
	_, _, p = cfg.lookup(filepath.Join(root, "x.pdf"))
	if len(p) != 1 || string(p[0]) != "outer" {
		t.Fatalf("passwords at the root: %q", p)
	}
}

func TestConfigEditsAreSeenAfterTheRecheckInterval(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1_000_000, 0)
	cfg := newDirConfig(root, func() time.Time { return now }, (&testLog{}).logf)
	pdf := filepath.Join(root, "a.pdf")
	if ig, _, _ := cfg.lookup(pdf); ig {
		t.Fatal("no config yet")
	}
	writeConf(t, root, "[ignore]\n*.pdf")
	if ig, _, _ := cfg.lookup(pdf); ig {
		t.Fatal("a new file may take up to the recheck interval to be noticed (no stat per lookup)")
	}
	now = now.Add(3 * time.Second)
	if ig, _, _ := cfg.lookup(pdf); !ig {
		t.Fatal("the new file must be picked up after the interval")
	}
	writeConf(t, root, "[ignore]\n*.txt # now something else")
	now = now.Add(3 * time.Second)
	if ig, _, _ := cfg.lookup(pdf); ig {
		t.Fatal("an edited file must be re-read")
	}
	os.Remove(filepath.Join(root, configFileName))
	now = now.Add(3 * time.Second)
	writeConf(t, root, "[ignore]\n*.pdf")
	now = now.Add(3 * time.Second)
	if ig, _, _ := cfg.lookup(pdf); !ig {
		t.Fatal("a re-created file must be picked up")
	}
}

func TestPathsOutsideTheRootAreNeverAffected(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	writeConf(t, root, "[ignore]\n*.pdf")
	cfg := newCfg(root, &testLog{})
	if ig, _, _ := cfg.lookup(filepath.Join(other, "a.pdf")); ig {
		t.Fatal("outside the root")
	}
	if ig, _, _ := (*dirConfig)(nil).lookup("/x/a.pdf"); ig {
		t.Fatal("nil config")
	}
}
