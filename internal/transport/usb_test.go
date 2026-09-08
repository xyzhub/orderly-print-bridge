//go:build !windows

package transport

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A regular file stands in for the device node: usblp is a character device the
// daemon only ever opens O_WRONLY and writes to, which is exactly what a plain
// file does.
func TestSendUSBWritesToTheDeviceNode(t *testing.T) {
	node := filepath.Join(t.TempDir(), "lp0")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte{0x1b, 0x40, 'h', 'i'}

	for _, target := range []string{"usb:" + node, "usb://" + node} {
		if err := os.Truncate(node, 0); err != nil {
			t.Fatal(err)
		}
		n, err := Send(target, payload)
		if err != nil {
			t.Fatalf("Send(%s): %v", target, err)
		}
		if n != len(payload) {
			t.Fatalf("Send(%s) wrote %d bytes, want %d", target, n, len(payload))
		}
		got, err := os.ReadFile(node)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(payload) {
			t.Fatalf("Send(%s) wrote %q", target, got)
		}
	}
}

// A usb target must never CREATE the node: a printer that is unplugged has to
// fail loudly, not swallow the receipt into a new regular file.
func TestSendUSBRefusesAMissingNode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "lp9")
	n, err := Send("usb:"+missing, []byte("x"))
	if err == nil {
		t.Fatal("writing to a device that is not there must fail")
	}
	if n != 0 {
		t.Fatalf("nothing reached a printer, so bytes written must be 0, got %d", n)
	}
	if !strings.Contains(err.Error(), "no such device") {
		t.Fatalf("the error must name the cause: %v", err)
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatal("a usb target created the file it was supposed to refuse")
	}
}

// The one error an operator on a PC install hits, and the one that must name
// its fix. Skipped as root, where the mode bits do not apply.
func TestSendUSBNamesTheLpGroupOnPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the permission branch is unreachable")
	}
	node := filepath.Join(t.TempDir(), "lp0")
	if err := os.WriteFile(node, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	_, err := Send("usb:"+node, []byte("x"))
	if err == nil {
		t.Fatal("an unwritable device node must fail")
	}
	if !strings.Contains(err.Error(), "`lp` group") {
		t.Fatalf("the permission error must name the lp group: %v", err)
	}
}

// A FIFO with no reader blocks in the kernel exactly the way a powered-off
// usblp node does; the open timeout is what keeps that from wedging a job.
func TestSendUSBTimesOutOnABlockedOpen(t *testing.T) {
	node := filepath.Join(t.TempDir(), "lp0")
	if err := syscall.Mkfifo(node, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	saved := USBOpenTimeout
	USBOpenTimeout = 150 * time.Millisecond
	t.Cleanup(func() { USBOpenTimeout = saved })

	start := time.Now()
	n, err := Send("usb:"+node, []byte("x"))
	if err == nil {
		t.Fatal("an open that never completes must fail, not hang")
	}
	if n != 0 {
		t.Fatalf("no bytes can have reached a device that never opened, got %d", n)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the open timeout did not fire: waited %s", elapsed)
	}
	if !strings.Contains(err.Error(), "powered on") {
		t.Fatalf("the timeout error must suggest the fix: %v", err)
	}
}

func TestSendUSBRejectsAnEmptyPath(t *testing.T) {
	if _, err := Send("usb:", []byte("x")); err == nil {
		t.Fatal("an empty usb path must fail")
	}
}
