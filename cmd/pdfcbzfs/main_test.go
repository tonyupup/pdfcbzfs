package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tonyupup/pdfcbzfs/internal/testpdf"
)

// The test binary doubles as the program, so the test can kill a real daemon.
func TestMain(m *testing.M) {
	if os.Getenv("PDFCBZFS_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func start(t *testing.T, lower, mnt string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-f", lower, mnt, "-o", "ro")
	cmd.Env = append(os.Environ(), "PDFCBZFS_AS_MAIN=1")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRestartOverStaleMount(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil || os.Getuid() != 0 {
		t.Skip("needs /dev/fuse and root")
	}
	lower, mnt := t.TempDir(), t.TempDir()
	pdf := testpdf.Classic([][]byte{testpdf.JPEG(1, 16, 16), testpdf.JPEG(2, 16, 16)}, testpdf.Opts{})
	if err := os.WriteFile(filepath.Join(lower, "book.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Unmount(mnt, syscall.MNT_DETACH) })
	// ReadDir, not Stat: the kernel keeps answering lookups of recently seen
	// names from its cache for a second after the daemon died.
	listed := func() bool {
		ents, err := os.ReadDir(mnt)
		return err == nil && len(ents) == 1 && ents[0].Name() == "book.cbz"
	}

	first := start(t, lower, mnt)
	waitFor(t, "first mount", listed)
	first.Process.Kill() // no unmount: the mount is left dead
	first.Wait()
	if _, err := os.Stat(mnt); !errors.Is(err, syscall.ENOTCONN) {
		t.Skipf("kernel did not leave a stale mount (stat: %v)", err)
	}

	second := start(t, lower, mnt)
	defer func() { second.Process.Signal(syscall.SIGTERM); second.Wait() }()
	waitFor(t, "second mount over the stale one", listed)
	// Mounting on top of the dead mount would also "work", but stack another
	// layer on it; the stale one must have been detached instead.
	if n := mountsAt(t, mnt); n != 1 {
		t.Fatalf("%d mounts at %s, want exactly 1 (stale mount not detached)", n, mnt)
	}
}

func mountsAt(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Skip("no /proc/self/mountinfo")
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 4 && f[4] == dir {
			n++
		}
	}
	return n
}
