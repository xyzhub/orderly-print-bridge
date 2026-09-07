package transport

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Counsel finding 4 begins here: Send must REPORT how many bytes reached the
// printer. transport.go used to discard conn.Write's count, which left the
// daemon unable to tell "nothing printed" from "half a receipt printed".
func TestSendReportsBytesWrittenOnAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.escpos")
	data := []byte("\x1b@hello ESC/POS")

	n, err := Send("file://"+path, data)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Send reported %d bytes, wrote %d", n, len(data))
	}
	on, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(on) != string(data) {
		t.Fatalf("file holds %q", on)
	}
}

func TestSendBarePathAndUSBScheme(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0x1b, 0x40}

	if n, err := Send(filepath.Join(dir, "bare"), data); err != nil || n != 2 {
		t.Fatalf("bare path: n=%d err=%v", n, err)
	}
	if n, err := Send("usb://"+filepath.Join(dir, "usbdev"), data); err != nil || n != 2 {
		t.Fatalf("usb scheme: n=%d err=%v", n, err)
	}
}

func TestSendReportsBytesWrittenOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer conn.Close()
		n, _ := io.Copy(io.Discard, conn)
		got <- int(n)
	}()

	data := make([]byte, 64<<10)
	for i := range data {
		data[i] = byte(i)
	}
	n, err := Send("tcp://"+ln.Addr().String(), data)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Send reported %d bytes, want %d", n, len(data))
	}
	if received := <-got; received != len(data) {
		t.Fatalf("the listener received %d bytes, want %d", received, len(data))
	}
}

func TestSendToADeadPortReportsZeroBytes(t *testing.T) {
	// Bind then close, so the port is almost certainly free and refusing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	n, err := Send("tcp://"+addr, []byte("data"))
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if n != 0 {
		t.Fatalf("a refused connection wrote %d bytes; it must report 0 so the daemon can retry", n)
	}
}

func TestSendRejectsUnknownSchemeAndEmptyPath(t *testing.T) {
	if _, err := Send("ipp://printer/queue", []byte("x")); err == nil {
		t.Fatal("expected an unsupported-scheme error")
	}
	if _, err := Send("file://", []byte("x")); err == nil {
		t.Fatal("expected an empty-path error")
	}
}

func TestQueryReturnsAPrinterReply(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 8)
		if _, err := conn.Read(buf); err != nil {
			return
		}
		_, _ = conn.Write([]byte("SRP-E300"))
	}()

	reply, err := Query("tcp://"+ln.Addr().String(), []byte{0x1d, 0x49, 0x01})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if string(reply) != "SRP-E300" {
		t.Fatalf("got %q", reply)
	}
}

// A printer that says nothing is not an identification — and not an error.
func TestQuerySilenceIsNotAnIdentification(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Accept and hold the connection open, answering nothing, so the query
	// hits its deadline rather than an EOF.
	conns := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conns <- conn
		}
	}()
	defer func() {
		select {
		case c := <-conns:
			c.Close()
		default:
		}
	}()

	reply, err := Query("tcp://"+ln.Addr().String(), []byte{0x1d, 0x49, 0x01})
	if err != nil {
		t.Fatalf("a silent printer must not be an error, got %v", err)
	}
	if len(reply) != 0 {
		t.Fatalf("got a reply from a silent printer: %q", reply)
	}
}

func TestQueryIsTCPOnly(t *testing.T) {
	_, err := Query("usb:///dev/usb/lp0", []byte{0x1d, 0x49, 0x01})
	if !errors.Is(err, ErrQueryUnsupported) {
		t.Fatalf("want ErrQueryUnsupported, got %v", err)
	}
	if !strings.Contains(ErrQueryUnsupported.Error(), "tcp") {
		t.Fatal("the error should name the supported scheme")
	}
}
