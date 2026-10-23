// Command pdfcbzfs mounts a passthrough FUSE filesystem that presents PDFs
// (one JPEG per page) as CBZ files.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tonyupup/pdfcbzfs/internal/fusefs"
	"github.com/tonyupup/pdfcbzfs/internal/view"
)

func main() {
	lower := flag.String("lower", "", "real directory to mirror (required)")
	pages := flag.Int("pages-per-cbz", 200, "pages per generated CBZ")
	mode := flag.String("pdf-mode", "auto", "auto: PDFs with <= pages-per-cbz pages become one .cbz in place, longer ones a directory of parts; folder: always a directory")
	allowOther := flag.Bool("allow-other", true, "let users other than the mounter access the mount (needed for knfsd)")
	ro := flag.Bool("ro", false, "mount read-only")
	debug := flag.Bool("debug", false, "log FUSE requests")
	verbose := flag.Bool("v", false, "log why PDFs are passed through unchanged")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s --lower DIR [flags] MOUNTPOINT\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *lower == "" || flag.NArg() != 1 || (*mode != "auto" && *mode != "folder") || *pages < 1 || *pages > 60000 {
		flag.Usage()
		os.Exit(2)
	}
	cfg := view.Config{PagesPerCBZ: *pages, Mode: view.Mode(*mode)}
	if *verbose {
		cfg.Logf = log.Printf
	}
	f, err := fusefs.New(*lower, cfg)
	if err != nil {
		log.Fatalf("lower: %v", err)
	}
	srv, err := f.Mount(flag.Arg(0), fusefs.MountOptions{AllowOther: *allowOther, ReadOnly: *ro, Debug: *debug})
	if err != nil {
		log.Fatalf("mount: %v", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		if err := srv.Unmount(); err != nil {
			log.Printf("unmount: %v", err)
		}
	}()
	log.Printf("pdfcbzfs: %s -> %s (pages-per-cbz=%d, mode=%s)", *lower, flag.Arg(0), *pages, *mode)
	srv.Wait()
}
