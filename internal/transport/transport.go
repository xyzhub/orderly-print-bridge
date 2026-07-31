// Package transport delivers a finished ESC/POS byte stream to a printer.
//
// Three target forms are supported, chosen by URI scheme:
//
//	tcp://HOST:9100     raw TCP socket to a network ESC/POS printer (universal)
//	usb:///dev/usb/lp0  a raw character device (Linux lp, macOS/BSD, or any path)
//	file:///path/out    write the bytes to a file (dry run / capture)
//
// A bare path with no scheme (e.g. /dev/usb/lp0 or ./out.bin) is treated as a
// raw device/file write. This keeps the common "just point it at the device"
// case terse.
package transport

import (
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

// Send delivers data to the printer identified by target.
func Send(target string, data []byte) error {
	switch {
	case strings.HasPrefix(target, "tcp://"):
		return sendTCP(strings.TrimPrefix(target, "tcp://"), data)
	case strings.HasPrefix(target, "usb://"):
		// usb:///dev/usb/lp0 -> /dev/usb/lp0  (strip scheme, keep leading slash)
		return sendDevice(strings.TrimPrefix(target, "usb://"), data)
	case strings.HasPrefix(target, "file://"):
		return sendDevice(strings.TrimPrefix(target, "file://"), data)
	case strings.Contains(target, "://"):
		return fmt.Errorf("unsupported printer scheme in %q (use tcp://, usb://, or file://)", target)
	default:
		// Bare path: raw device or file.
		return sendDevice(target, data)
	}
}

func sendTCP(hostPort string, data []byte) error {
	// Default to the JetDirect/RAW port 9100 if none given.
	if !strings.Contains(hostPort, ":") {
		hostPort += ":9100"
	}
	conn, err := net.DialTimeout("tcp", hostPort, DialTimeout)
	if err != nil {
		return fmt.Errorf("connect to printer %s: %w", hostPort, err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write to printer %s: %w", hostPort, err)
	}
	return nil
}

func sendDevice(path string, data []byte) error {
	if path == "" {
		return fmt.Errorf("empty device/file path")
	}
	// O_WRONLY works for both a real character device and a plain file.
	// Create the file if it does not exist (file:// capture); a device node
	// already exists so O_CREATE is harmless there.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
