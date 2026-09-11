package local

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestGroupScanToleratesProcessDisappearanceOnly(t *testing.T) {
	root := t.TempDir()
	for _, pid := range []string{"101", "102"} {
		if e := os.Mkdir(filepath.Join(root, pid), 0700); e != nil {
			t.Fatal(e)
		}
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		t.Fatal(e)
	}
	for _, gone := range []error{syscall.ENOENT, syscall.ESRCH} {
		reads := 0
		alive, err := scanProcessGroup(entries, 102, func(pid int) ([]string, error) {
			reads++
			if pid == 101 {
				return nil, &os.PathError{Op: "read", Path: "/proc/101/stat", Err: gone}
			}
			return []string{"S", "1", "102"}, nil
		})
		if err != nil || !alive || reads != 2 {
			t.Fatalf("lost remaining group after disappearing process: alive=%v reads=%d err=%v", alive, reads, err)
		}
	}
	alive, err := scanProcessGroup(entries, 102, func(int) ([]string, error) { return nil, syscall.EACCES })
	if !alive || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("permission failure cannot prove group stopped: %v %v", alive, err)
	}
}
