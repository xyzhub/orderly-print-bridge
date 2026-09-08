package discover

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func addrs(cidrs ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		out := make([]net.Addr, 0, len(cidrs))
		for _, c := range cidrs {
			ip, ipnet, err := net.ParseCIDR(c)
			if err != nil {
				return nil, err
			}
			ipnet.IP = ip
			out = append(out, ipnet)
		}
		return out, nil
	}
}

// The acceptance case from the brief: nothing answers, so the sweep returns an
// empty slice and NO error. A venue where every printer is off is not a failure.
func TestZeroRespondersIsEmptyAndNotAnError(t *testing.T) {
	got, err := Sweep(context.Background(), Options{
		Interfaces: addrs("192.168.1.20/24"),
		Dial:       func(context.Context, string) error { return errors.New("connection refused") },
		GOOS:       "linux",
		USBRoot:    t.TempDir(), // an empty root: no /dev/usb/lp*
	})
	if err != nil {
		t.Fatalf("a silent network must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no candidates, got %v", got)
	}
	if got == nil {
		t.Fatal("the payload must be an empty slice, not nil, so the heartbeat sends []")
	}
}

func TestSweepIdentifiesResponders(t *testing.T) {
	dialed := map[string]bool{}
	var mu sync.Mutex // the sweep dials 32 at a time; the RECORDER needs the lock
	got, err := Sweep(context.Background(), Options{
		Interfaces: addrs("192.168.1.20/24"),
		Dial: func(_ context.Context, address string) error {
			mu.Lock()
			dialed[address] = true
			mu.Unlock()
			if address == "192.168.1.50:9100" || address == "192.168.1.77:9100" {
				return nil
			}
			return errors.New("refused")
		},
		Probe: func(target string, _ []byte) ([]byte, error) {
			if strings.Contains(target, "192.168.1.50") {
				return []byte("TM-T20III"), nil
			}
			return nil, nil // silence: answered on 9100 but did not identify
			// (an office LaserJet looks exactly like this)
		},
		GOOS:    "linux",
		USBRoot: t.TempDir(),
		Now:     func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %v", len(got), got)
	}
	byAddr := map[string]string{}
	for _, c := range got {
		if c.Transport != "tcp" {
			t.Fatalf("%s: transport = %q", c.Address, c.Transport)
		}
		if c.LastSeenAt.IsZero() {
			t.Fatalf("%s: lastSeenAt is unset", c.Address)
		}
		byAddr[c.Address] = c.Identity
	}
	if byAddr["192.168.1.50:9100"] != "TM-T20III" {
		t.Fatalf("the identified printer lost its identity: %v", byAddr)
	}
	if id, ok := byAddr["192.168.1.77:9100"]; !ok || id != "" {
		t.Fatalf("a silent responder must be listed WITHOUT an identity: %v", byAddr)
	}
	// The sweep must never have looked outside its own /24, and never at itself.
	for address := range dialed {
		if !strings.HasPrefix(address, "192.168.1.") {
			t.Fatalf("swept outside the local /24: %s", address)
		}
	}
	if dialed["192.168.1.20:9100"] {
		t.Fatal("the box swept its own address")
	}
	if len(dialed) != 253 {
		t.Fatalf("a /24 minus ourselves is 253 hosts, dialled %d", len(dialed))
	}
}

// A flat /16 must never become a 65,000-host scan.
func TestAWideNetworkIsNarrowedToA24(t *testing.T) {
	hosts, err := LocalHosts(addrs("10.0.5.9/16"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 253 {
		t.Fatalf("want the /24 around us (253 hosts), got %d", len(hosts))
	}
	for _, h := range hosts {
		if !strings.HasPrefix(h, "10.0.5.") {
			t.Fatalf("host %s is outside our /24", h)
		}
	}
}

func TestLoopbackIsNotSwept(t *testing.T) {
	hosts, err := LocalHosts(addrs("127.0.0.1/8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Fatalf("loopback must not be swept, got %d hosts", len(hosts))
	}
}

// The USB half (owner amendment, 2026-09-08): the node plus whatever sysfs can
// tell us about it.
func TestUSBScanReadsSysfsMetadata(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "dev", "usb"))
	mustWrite(t, filepath.Join(root, "dev", "usb", "lp0"), "")

	// sysfs, faithfully: /sys/class/usbmisc/lp0/device is a SYMLINK to the USB
	// interface directory, and `..` from there is the device directory holding
	// the ids. That is why the reader concatenates instead of filepath.Join-ing.
	usbDev := filepath.Join(root, "sys", "bus", "usb", "devices", "1-1")
	iface := filepath.Join(usbDev, "1-1:1.0")
	mustMkdir(t, iface)
	mustWrite(t, filepath.Join(usbDev, "idVendor"), "04b8\n")
	mustWrite(t, filepath.Join(usbDev, "idProduct"), "0e15\n")
	mustWrite(t, filepath.Join(usbDev, "manufacturer"), "EPSON\n")
	mustWrite(t, filepath.Join(usbDev, "product"), "TM-T20III\n")
	classDir := filepath.Join(root, "sys", "class", "usbmisc", "lp0")
	mustMkdir(t, classDir)
	if err := os.Symlink(iface, filepath.Join(classDir, "device")); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}

	got := scanUSB(&Options{GOOS: "linux", USBRoot: root, Now: time.Now})
	if len(got) != 1 {
		t.Fatalf("want one USB candidate, got %v", got)
	}
	c := got[0]
	if want := "usb:" + filepath.Join(root, "dev", "usb", "lp0"); c.Address != want {
		t.Fatalf("address = %q, want %q", c.Address, want)
	}
	if c.Transport != "usb" {
		t.Fatalf("transport = %q", c.Transport)
	}
	if c.Model != "EPSON TM-T20III" {
		t.Fatalf("model = %q", c.Model)
	}
	if c.VendorID != "04b8" || c.ProductID != "0e15" {
		t.Fatalf("usb ids = %q/%q", c.VendorID, c.ProductID)
	}
	if c.LastSeenAt.IsZero() {
		t.Fatal("lastSeenAt is unset")
	}
}

// Inside a container the node is passed through but /sys is not. The node is
// the part that can be printed to, so it must still be reported.
func TestUSBScanSurvivesMissingSysfs(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "dev", "usb"))
	mustWrite(t, filepath.Join(root, "dev", "usb", "lp0"), "")

	got := scanUSB(&Options{GOOS: "linux", USBRoot: root, Now: time.Now})
	if len(got) != 1 {
		t.Fatalf("want the node reported anyway, got %v", got)
	}
	if got[0].Model != "" || got[0].VendorID != "" {
		t.Fatalf("want no metadata, got %+v", got[0])
	}
}

// USB discovery is Linux-only in this release; a mac dev box must not invent
// candidates from /dev.
func TestUSBScanIsLinuxOnly(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "dev", "usb"))
	mustWrite(t, filepath.Join(root, "dev", "usb", "lp0"), "")
	if got := scanUSB(&Options{GOOS: "darwin", USBRoot: root}); got != nil {
		t.Fatalf("want nothing off Linux, got %v", got)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
