// Command pdfcbzfs mounts a passthrough FUSE filesystem that presents PDFs
// (one image per page) as CBZ files.
//
// It follows the usual FUSE command line, so it also works as a mount(8)
// helper (mount -t fuse.pdfcbzfs, /etc/fstab):
//
//	pdfcbzfs LOWER MOUNTPOINT [-o opt[,opt...]] [-f] [-d] [-v]
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/tonyupup/pdfcbzfs/internal/fusefs"
	"github.com/tonyupup/pdfcbzfs/internal/view"
)

const readyFD = "PDFCBZFS_READY_FD"

const usage = `usage: pdfcbzfs LOWER MOUNTPOINT [options]

general options:
    -o opt,[opt...]   mount options (see below)
    -f                stay in the foreground
    -d                debug: log FUSE requests (implies -f)
    -v                log why PDFs are passed through unchanged
    -h                show this help

mount options:
    ro, rw            read-only / read-write (default rw)
    allow_other       let users other than the mounter access the mount
                      (needed for knfsd and for media servers running as another user)
    pages_per_cbz=N   pages per generated CBZ (default 200)
    pdf_mode=MODE     auto (default): short PDFs become one .cbz in place,
                      longer ones a directory of parts; folder: always a directory
    verbose, debug    same as -v / -d
    other standard mount options (nosuid, nodev, noatime, default_permissions,
    fsname=, subtype=, x-*, ...) are accepted and ignored
`

type config struct {
	lower, mountpoint string
	pages             int
	mode              view.Mode
	allowOther        bool
	readOnly          bool
	foreground        bool
	debug, verbose    bool
}

func main() {
	c, err := parseArgs(os.Args[1:])
	if err != nil {
		if err != errHelp {
			fmt.Fprintf(os.Stderr, "pdfcbzfs: %v\n", err)
		}
		fmt.Fprint(os.Stderr, usage)
		if err == errHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if !c.foreground {
		os.Exit(daemonize())
	}
	run(c)
}

var errHelp = fmt.Errorf("help")

func parseArgs(args []string) (*config, error) {
	c := &config{pages: 200, mode: view.Mode("auto")}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			return nil, errHelp
		case a == "-f":
			c.foreground = true
		case a == "-d":
			c.debug, c.foreground = true, true
		case a == "-v":
			c.verbose = true
		case a == "-o":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("-o needs an argument")
			}
			if err := c.parseOpts(args[i]); err != nil {
				return nil, err
			}
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			if err := c.parseOpts(a[2:]); err != nil {
				return nil, err
			}
		case strings.HasPrefix(a, "-") && a != "-":
			return nil, fmt.Errorf("unknown option %q", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return nil, fmt.Errorf("need exactly LOWER and MOUNTPOINT")
	}
	// mount.fuse passes the fstab source as "pdfcbzfs#/dir" in some versions.
	c.lower = strings.TrimPrefix(pos[0], "pdfcbzfs#")
	c.mountpoint = pos[1]
	return c, nil
}

func (c *config) parseOpts(s string) error {
	for _, o := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(o, "=")
		switch k {
		case "":
		case "ro":
			c.readOnly = true
		case "rw":
			c.readOnly = false
		case "allow_other":
			c.allowOther = true
		case "pages_per_cbz":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 60000 {
				return fmt.Errorf("pages_per_cbz must be 1..60000, got %q", v)
			}
			c.pages = n
		case "pdf_mode":
			if v != "auto" && v != "folder" {
				return fmt.Errorf("pdf_mode must be auto or folder, got %q", v)
			}
			c.mode = view.Mode(v)
		case "debug":
			c.debug, c.foreground = true, true
		case "verbose":
			c.verbose = true
		case "defaults", "suid", "nosuid", "dev", "nodev", "exec", "noexec",
			"async", "sync", "atime", "noatime", "relatime", "diratime", "nodiratime",
			"auto", "noauto", "nofail", "_netdev", "user", "nouser", "users",
			"default_permissions", "fsname", "subtype", "fd", "rootmode", "user_id", "group_id":
			// accepted for mount(8) compatibility
		default:
			if strings.HasPrefix(k, "x-") {
				continue
			}
			return fmt.Errorf("unknown mount option %q", o)
		}
	}
	return nil
}

// run mounts and serves until unmounted. When started by daemonize it reports
// success or the mount error through the ready pipe.
func run(c *config) {
	var ready *os.File
	if v := os.Getenv(readyFD); v != "" {
		ready = os.NewFile(3, "ready")
		os.Unsetenv(readyFD)
	}
	fail := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if ready != nil {
			io.WriteString(ready, msg)
			ready.Close()
			os.Exit(1)
		}
		log.Fatal(msg)
	}

	cfg := view.Config{PagesPerCBZ: c.pages, Mode: c.mode}
	if c.verbose {
		cfg.Logf = log.Printf
	}
	f, err := fusefs.New(c.lower, cfg)
	if err != nil {
		fail("lower: %v", err)
	}
	srv, err := f.Mount(c.mountpoint, fusefs.MountOptions{AllowOther: c.allowOther, ReadOnly: c.readOnly, Debug: c.debug})
	if err != nil {
		fail("mount: %v", err)
	}
	if ready != nil {
		ready.Close() // EOF with no data == success
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		if err := srv.Unmount(); err != nil {
			log.Printf("unmount: %v", err)
		}
	}()
	log.Printf("pdfcbzfs: %s -> %s (pages_per_cbz=%d, pdf_mode=%s)", c.lower, c.mountpoint, c.pages, c.mode)
	srv.Wait()
}

// daemonize re-executes the program in the background with -f, waits until it
// has mounted (or failed), and returns the exit status for the parent.
func daemonize() int {
	r, w, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pdfcbzfs: %v\n", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pdfcbzfs: %v\n", err)
		return 1
	}
	devnull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	cmd := exec.Command(exe, append([]string{"-f"}, os.Args[1:]...)...)
	cmd.Env = append(os.Environ(), readyFD+"=3")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "pdfcbzfs: %v\n", err)
		return 1
	}
	w.Close()
	msg, _ := io.ReadAll(r)
	if len(msg) > 0 {
		fmt.Fprintf(os.Stderr, "pdfcbzfs: %s\n", msg)
		cmd.Wait()
		return 1
	}
	return 0
}
