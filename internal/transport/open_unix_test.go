//go:build unix

package transport

import (
	"os"
	"path/filepath"
	"testing"
)

// The whole fix is one bit: the descriptor the USB writer holds must NOT be
// non-blocking, or usblp's close() cancels the tail of the job (the cut).
func TestOpenBlockingIsBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openBlocking(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	nb, err := isNonBlocking(f)
	if err != nil {
		t.Fatal(err)
	}
	if nb {
		t.Fatal("openBlocking returned a non-blocking descriptor — usblp will cancel in-flight writes on close")
	}
	if n, err := f.Write([]byte("cut")); err != nil || n != 3 {
		t.Fatalf("write through the blocking descriptor: n=%d err=%v", n, err)
	}
}
