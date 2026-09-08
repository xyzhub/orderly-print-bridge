// Package transport delivers a finished ESC/POS byte stream to a printer.
//
// Three target forms are supported, chosen by URI scheme:
//
//	tcp://HOST:9100     raw TCP socket to a network ESC/POS printer (universal)
//	usb:/dev/usb/lp0    a USB printer-class device node (Linux usblp, macOS/BSD)
//	usb:///dev/usb/lp0  the same thing with an authority-style scheme
//	file:///path/out    write the bytes to a file (dry run / capture)
//
// A bare path with no scheme (e.g. /dev/usb/lp0 or ./out.bin) still works —
// that is the spelling a `DevicePrinter` row carried before `usb:` existed, and
// the one an operator types. A bare path under /dev/ takes the device path
// (never created); anything else is a plain file write.
//
// `usb:` and `file:` differ in one deliberate way: a usb target is NEVER
// created. A device node exists or it does not, and creating a regular file at
// /dev/usb/lp0 because the printer was unplugged would turn "the printer is
// off" into a silent success that swallows every receipt.
package transport

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// DialTimeout bounds how long we wait to reach a network printer.
const DialTimeout = 8 * time.Second

// WriteTimeout bounds how long a network write may block (a printer with a full
// buffer / paper out can otherwise hang the send indefinitely).
const WriteTimeout = 30 * time.Second

// Send delivers data to the printer identified by target and returns THE
// NUMBER OF BYTES THAT REACHED IT.
//
// The byte count is not a nicety. A TCP write that stalls mid-stream errors
// after part of the receipt has already been printed; if the daemon treated
// that as retryable it would print a second legal invoice (counsel finding 4).
// The count is what lets the caller ack a terminal `failed{partial}` instead.
// On error the count is the bytes written before it, and may be > 0.
func Send(target string, data []byte) (int, error) {
	switch {
	case strings.HasPrefix(target, "tcp://"):
		return sendTCP(strings.TrimPrefix(target, "tcp://"), data)
	case strings.HasPrefix(target, "usb://"):
		// usb:///dev/usb/lp0 -> /dev/usb/lp0  (strip scheme, keep leading slash)
		return sendUSB(strings.TrimPrefix(target, "usb://"), data)
	case strings.HasPrefix(target, "usb:"):
		// usb:/dev/usb/lp0 — the form the discovery sweep reports, so a manager
		// can paste a swept address straight into the printer form.
		return sendUSB(strings.TrimPrefix(target, "usb:"), data)
	case strings.HasPrefix(target, "file://"):
		return sendDevice(strings.TrimPrefix(target, "file://"), data)
	case strings.Contains(target, "://"):
		return 0, fmt.Errorf("unsupported printer scheme in %q (use tcp://, usb://, or file://)", target)
	case strings.HasPrefix(target, "/dev/"):
		// A bare device path — the spelling a printer row carried before the
		// `usb:` scheme existed, and the one an operator types. It gets the
		// hardened device path, not the file path: nothing may ever CREATE a
		// node under /dev.
		return sendUSB(target, data)
	default:
		// Bare path: raw file (a capture sink, a dry run).
		return sendDevice(target, data)
	}
}

func sendTCP(hostPort string, data []byte) (int, error) {
	conn, err := dialTCP(hostPort)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return 0, fmt.Errorf("set write deadline: %w", err)
	}
	// net.Conn.Write reports how much of data made it out before any error;
	// that count is propagated verbatim.
	n, err := conn.Write(data)
	if err != nil {
		return n, fmt.Errorf("write to printer %s (%d of %d bytes sent): %w", hostPort, n, len(data), err)
	}
	return n, nil
}

func dialTCP(hostPort string) (net.Conn, error) {
	// Default to the JetDirect/RAW port 9100 if none given.
	if !strings.Contains(hostPort, ":") {
		hostPort += ":9100"
	}
	conn, err := net.DialTimeout("tcp", hostPort, DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("connect to printer %s: %w", hostPort, err)
	}
	return conn, nil
}

func sendDevice(path string, data []byte) (int, error) {
	if path == "" {
		return 0, fmt.Errorf("empty device/file path")
	}
	// O_WRONLY works for both a real character device and a plain file.
	// Create the file if it does not exist (file:// capture); a device node
	// already exists so O_CREATE is harmless there.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	n, err := f.Write(data)
	if err != nil {
		return n, fmt.Errorf("write %s (%d of %d bytes sent): %w", path, n, len(data), err)
	}
	return n, nil
}

// USBOpenTimeout bounds opening a device node. A var, not a const, so the test
// does not have to wait eight seconds on a FIFO with no reader.
//
// The timeout is not decoration: opening a usblp node whose printer is powered
// off, or a FIFO with nothing on the other end, blocks in the kernel with no
// deadline of its own, and a blocked open inside the daemon's 60 s job budget
// is a job that expires without ever saying why.
var USBOpenTimeout = 8 * time.Second

// sendUSB writes an ESC/POS stream to a USB printer-class device node.
func sendUSB(path string, data []byte) (int, error) {
	if path == "" {
		return 0, fmt.Errorf("empty USB device path (expected usb:/dev/usb/lp0)")
	}
	f, err := openDeviceWithTimeout(path, USBOpenTimeout)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Best-effort: a character device is often not pollable, in which case the
	// runtime answers ErrNoDeadline and the write is bounded by the daemon's
	// 60 s job budget instead. Not an error either way.
	if err := f.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil && !errors.Is(err, os.ErrNoDeadline) {
		return 0, fmt.Errorf("set write deadline on %s: %w", path, err)
	}
	n, err := f.Write(data)
	if err != nil {
		return n, fmt.Errorf("write %s (%d of %d bytes sent): %w", path, n, len(data), err)
	}
	return n, nil
}

// openDeviceWithTimeout opens path O_WRONLY, giving up after timeout. A late
// success is closed by the goroutine so an unplugged-then-replugged printer
// cannot leave a stray write handle open.
func openDeviceWithTimeout(path string, timeout time.Duration) (*os.File, error) {
	type opened struct {
		f   *os.File
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		ch <- opened{f, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			return nil, describeOpenError(path, got.err)
		}
		return got.f, nil
	case <-time.After(timeout):
		go func() {
			if late := <-ch; late.f != nil {
				late.f.Close()
			}
		}()
		return nil, fmt.Errorf("open %s: the device did not accept a connection within %s "+
			"(is the printer powered on and the cable seated?)", path, timeout)
	}
}

// describeOpenError turns the two failures an operator actually hits into
// sentences that name the fix.
func describeOpenError(path string, err error) error {
	switch {
	case errors.Is(err, os.ErrPermission):
		// The systemd unit runs as root on a box, but a client's PC install may
		// not — and "permission denied" alone sends the operator nowhere.
		return fmt.Errorf("open %s: permission denied — the account running the bridge must be in the `lp` group "+
			"(sudo usermod -aG lp $USER, then log out and back in), or run the bridge as root: %w", path, err)
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("open %s: no such device — the printer is unplugged, powered off, "+
			"or the usblp kernel module is not loaded: %w", path, err)
	default:
		return fmt.Errorf("open %s: %w", path, err)
	}
}

// QueryTimeout bounds an identity query. A printer that has not answered in
// two seconds is not going to.
const QueryTimeout = 2 * time.Second

// ErrQueryUnsupported is returned when the target cannot be interrogated.
var ErrQueryUnsupported = errors.New("identity query is only supported on tcp:// targets")

// Query sends a short command to a network printer and returns whatever it
// answers within QueryTimeout. It exists for the ESC/POS identity query
// (escpos.CmdIdentity, `GS I n`): port 9100 is JetDirect, so "the only host
// answering on 9100" can just as easily be an office LaserJet, and the welcome
// slip must never be sent blind (counsel finding 6).
//
// An empty reply with a nil error means the target accepted the query but said
// nothing — that is NOT an identification.
func Query(target string, cmd []byte) ([]byte, error) {
	if !strings.HasPrefix(target, "tcp://") {
		return nil, ErrQueryUnsupported
	}
	conn, err := dialTCP(strings.TrimPrefix(target, "tcp://"))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(QueryTimeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if _, err := conn.Write(cmd); err != nil {
		return nil, fmt.Errorf("write identity query: %w", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n > 0 {
		return buf[:n], nil
	}
	if err != nil {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			// Silence is a legitimate answer: no reply, no identification.
			return nil, nil
		}
		return nil, err
	}
	return nil, nil
}
