//go:build unix

package transport

import (
	"os"
	"runtime"
	"syscall"
)

// openBlocking opens a device node for writing with a BLOCKING descriptor.
//
// Why not os.OpenFile: Go registers a character device with its poller and
// switches the descriptor to non-blocking. The usblp driver then returns from
// write() as soon as the bytes are queued in USB request blocks — and on
// close() it CANCELS every request still in flight. A large receipt therefore
// reached the printer minus its tail: the four line feeds and the cut command.
// Plain `cat > /dev/usb/lp0` cuts because its descriptor is blocking, so
// write() returns only after the printer has taken the bytes. Proven on the
// owner's Bixolon SRP-E300, 2026-09-08: identical bytes, cat cut, the daemon
// did not.
//
// syscall.Open without O_NONBLOCK gives a blocking descriptor, and os.NewFile
// leaves a blocking descriptor out of the poller, so Write returns only when
// the kernel has handed everything to the device. SetWriteDeadline then answers
// ErrNoDeadline, which sendUSB already tolerates.
func openBlocking(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	// The trade-off of a blocking write: a printer that is out of paper or has
	// its cover open would hold write() open until someone reloads it, past
	// the daemon's 60 s job budget, and the job would then print anyway when
	// the lid closes. usblp's LPABORT switch makes it return ENOSPC on a paper
	// error instead, so the write fails fast, the job is retried later, and
	// close() has nothing dangling. Linux-only ioctl (lp.h LPABORT = 0x0604,
	// arg 1 = abort on error); harmless to skip elsewhere or on a plain file.
	if runtime.GOOS == "linux" {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), lpAbort, 1)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// lpAbort is LPABORT from <linux/lp.h>: 0x0604.
const lpAbort = 0x0604

// isNonBlocking reports whether fd carries O_NONBLOCK — the test's probe.
func isNonBlocking(f *os.File) (bool, error) {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFL, 0)
	if errno != 0 {
		return false, errno
	}
	return flags&syscall.O_NONBLOCK != 0, nil
}
