package view

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A directory may hold a configuration file, .pdf2cbz.conf. It applies to the
// directory it is in and everything below it; inner files override outer ones.
// It is an ordinary file of the mounted directory (visible, editable through
// the mount) and a change is picked up within a couple of seconds.
//
// The file has sections; lines outside a section are reported and skipped:
//
//	[ignore]                  PDFs that must not be converted (they stay PDFs)
//	*.draft.pdf
//	课本/
//	!keep.pdf
//
//	[passwords]               default passwords, "pattern = password"
//	课本/*.pdf = 123456
//
// Patterns use the syntax of .gitignore, matched case-insensitively:
//
//	# comment                blank lines are skipped
//	*.pdf   ?   [0-9]        wildcards (never match "/")
//	**                       any number of directories: "a/**/b.pdf", "old/**"
//	name                     a file or directory called name at any depth
//	/name  or  dir/name      relative to the directory holding the file
//	name/                    only a directory (and so everything below it)
//	!pattern                 ([ignore] only) re-include what an earlier line excluded
//	\#name  \!name           a leading # or ! that is part of the name
//
// In [ignore] a PDF below an excluded directory cannot be re-included, as in
// git. In [passwords] a pattern that contains "=" can be written in double
// quotes, and so can a password with leading or trailing spaces:
//
//	"a=b*.pdf" = "  secret  "
//
// Every matching password is tried, the most specific first, after the
// password in NAME.pdf.passwd.
const configFileName = ".pdf2cbz.conf"

// configRecheck is how long parsed files are trusted before they are looked
// at again, so edits take effect within seconds without a stat of every file
// on every lookup (a network round trip on NFS).
const configRecheck = 2 * time.Second

type rule struct {
	segs     []string // lower-cased pattern split at "/"
	neg      bool
	dirOnly  bool
	anchored bool // matched against the whole relative path, not just a name
	src      string
}

// confFile is one parsed .pdf2cbz.conf.
type confFile struct {
	file      string // path, for messages
	depth     int    // number of path segments of its directory below the root
	ignores   []rule
	passwords []pwRule
}

// pwRule is a [passwords] line.
type pwRule struct {
	rule
	password string
}

type dirRules struct {
	checked time.Time
	sig     string // size and mtime of the file read, to skip re-parsing
	conf    *confFile
}

type dirConfig struct {
	root string
	now  func() time.Time
	logf func(format string, a ...any)

	mu   sync.Mutex
	dirs map[string]*dirRules
}

func newDirConfig(root string, now func() time.Time, logf func(string, ...any)) *dirConfig {
	if root == "" {
		return nil
	}
	return &dirConfig{root: filepath.Clean(root), now: now, logf: logf, dirs: map[string]*dirRules{}}
}

// lookup says whether the PDF at pdfPath is excluded (and by which rule) and
// lists the default passwords that apply to it, most specific first.
func (c *dirConfig) lookup(pdfPath string) (ignored bool, why string, passwords [][]byte) {
	if c == nil {
		return false, "", nil
	}
	rel, err := filepath.Rel(c.root, pdfPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false, "", nil
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	segs := make([]string, len(parts))
	for i, p := range parts {
		segs[i] = strings.ToLower(p)
	}

	// the config files from the mount root down to the PDF's directory
	var files []*confFile
	for i := 0; i < len(parts); i++ {
		if d := c.load(filepath.Join(append([]string{c.root}, parts[:i]...)...), i); d.conf != nil {
			files = append(files, d.conf)
		}
	}
	if len(files) == 0 {
		return false, "", nil
	}
	ignored, why = excluded(segs, files)
	seen := map[string]bool{}
	for fi := len(files) - 1; fi >= 0; fi-- { // inner files first
		f := files[fi]
		for ri := len(f.passwords) - 1; ri >= 0; ri-- { // later lines first
			r := f.passwords[ri]
			if r.appliesTo(segs, f.depth) && !seen[r.password] {
				seen[r.password] = true
				passwords = append(passwords, []byte(r.password))
			}
		}
	}
	return ignored, why, passwords
}

// excluded evaluates the [ignore] sections for the path made of segs: every
// directory prefix first (an excluded directory takes everything below it),
// then the file itself.
func excluded(segs []string, files []*confFile) (bool, string) {
	for k := 1; k <= len(segs); k++ {
		isDir := k < len(segs)
		out, why := false, ""
		for _, f := range files {
			if k <= f.depth {
				continue // this prefix is above the directory of the file
			}
			sub := segs[f.depth:k]
			for _, r := range f.ignores {
				if r.matches(sub, isDir) {
					out, why = !r.neg, fmt.Sprintf("%s [ignore]: %s", f.file, r.src)
				}
			}
		}
		if out {
			return true, why
		}
	}
	return false, ""
}

// appliesTo reports whether a password rule covers the file: its own name or
// any directory above it below the config file's directory.
func (r pwRule) appliesTo(segs []string, depth int) bool {
	for k := depth + 1; k <= len(segs); k++ {
		if r.matches(segs[depth:k], k < len(segs)) {
			return true
		}
	}
	return false
}

// load returns the parsed config file of one directory (cached briefly).
func (c *dirConfig) load(dir string, depth int) *dirRules {
	now := c.now()
	c.mu.Lock()
	d := c.dirs[dir]
	if d != nil && now.Sub(d.checked) < configRecheck {
		c.mu.Unlock()
		return d
	}
	c.mu.Unlock()

	p := filepath.Join(dir, configFileName)
	sig := ""
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		sig = fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if d != nil && d.sig == sig {
		d.checked = now // unchanged: keep the parsed rules
		return d
	}
	nd := &dirRules{checked: now, sig: sig}
	if sig != "" {
		nd.conf = c.parseFile(p, depth)
	}
	c.dirs[dir] = nd
	return nd
}

func (c *dirConfig) parseFile(p string, depth int) *confFile {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	cf := &confFile{file: p, depth: depth}
	section := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if n == 1 {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf") // UTF-8 byte order mark
		}
		if name, ok := sectionName(line); ok {
			section = name
			continue
		}
		var err error
		switch section {
		case "ignore":
			var r rule
			var ok bool
			if r, ok, err = parseRule(line); ok && err == nil {
				cf.ignores = append(cf.ignores, r)
			}
		case "passwords":
			var r pwRule
			var ok bool
			if r, ok, err = parsePasswordRule(line); ok && err == nil {
				cf.passwords = append(cf.passwords, r)
			}
		default:
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
				err = fmt.Errorf("outside a known section ([ignore] or [passwords])")
			}
		}
		if err != nil {
			c.logf("config file %s line %d: %v (line skipped)", p, n, err) // never the password itself
		}
	}
	if len(cf.ignores) == 0 && len(cf.passwords) == 0 {
		return nil
	}
	return cf
}

// sectionName recognises "[ignore]" and "[passwords]" ("[password]" too). Any
// other bracketed line is a pattern, such as the character class "[0-9]*.pdf".
func sectionName(line string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(line))
	switch t {
	case "[ignore]":
		return "ignore", true
	case "[passwords]", "[password]":
		return "passwords", true
	}
	return "", false
}

// parseRule parses one [ignore] line; ok is false for blank lines and comments.
func parseRule(line string) (r rule, ok bool, err error) {
	if line == "" || strings.HasPrefix(line, "#") {
		return r, false, nil
	}
	r.src = line
	pat := line
	switch {
	case strings.HasPrefix(pat, `\#`), strings.HasPrefix(pat, `\!`):
		pat = pat[1:]
	case strings.HasPrefix(pat, "!"):
		r.neg, pat = true, pat[1:]
	}
	return finishPattern(r, pat, line)
}

func finishPattern(r rule, pat, line string) (rule, bool, error) {
	if strings.HasSuffix(pat, "/") {
		r.dirOnly, pat = true, strings.TrimRight(pat, "/")
	}
	if strings.HasPrefix(pat, "/") {
		r.anchored, pat = true, strings.TrimLeft(pat, "/")
	}
	if pat == "" {
		return r, false, nil
	}
	pat = strings.ToLower(pat)
	r.segs = strings.Split(pat, "/")
	if len(r.segs) > 1 {
		r.anchored = true
	}
	for _, s := range r.segs {
		if s == "" {
			return r, false, fmt.Errorf("empty path segment in the pattern")
		}
		if _, err := path.Match(s, ""); err != nil {
			return r, false, fmt.Errorf("bad pattern: %v", err)
		}
	}
	return r, true, nil
}

// parsePasswordRule parses "pattern = password".
func parsePasswordRule(line string) (r pwRule, ok bool, err error) {
	line = strings.TrimLeft(line, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return r, false, nil
	}
	var pat, rest string
	if strings.HasPrefix(line, `"`) {
		end := strings.Index(line[1:], `"`)
		if end < 0 {
			return r, false, fmt.Errorf("unterminated quote")
		}
		pat, rest = line[1:1+end], strings.TrimLeft(line[2+end:], " \t")
		if !strings.HasPrefix(rest, "=") {
			return r, false, fmt.Errorf(`expected "=" after the pattern`)
		}
		rest = rest[1:]
	} else {
		i := strings.Index(line, "=")
		if i < 0 {
			return r, false, fmt.Errorf(`expected "pattern = password"`)
		}
		pat, rest = strings.TrimRight(line[:i], " \t"), line[i+1:]
	}
	pw := strings.TrimSpace(rest)
	if len(pw) >= 2 && strings.HasPrefix(pw, `"`) && strings.HasSuffix(pw, `"`) {
		pw = pw[1 : len(pw)-1] // quotes keep spaces
	}
	if pw == "" {
		return r, false, fmt.Errorf("empty password")
	}
	if strings.HasPrefix(pat, "!") {
		return r, false, fmt.Errorf(`"!" has no meaning in a password file`)
	}
	pat = strings.TrimPrefix(pat, `\`) // \# \! escapes
	r.src, r.password = pat, pw
	rr, ok, err := finishPattern(r.rule, pat, line)
	r.rule = rr
	return r, ok, err
}

// matches reports whether the rule applies to rel (lower-cased segments below
// the config file's directory; the last one is the thing being tested).
func (r rule) matches(rel []string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if !r.anchored {
		ok, _ := path.Match(r.segs[0], rel[len(rel)-1])
		return ok
	}
	return matchSegs(r.segs, rel)
}

// matchSegs matches pattern segments against path segments; "**" stands for
// any number of whole segments.
func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
