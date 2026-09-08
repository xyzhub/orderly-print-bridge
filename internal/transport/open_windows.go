//go:build windows

package transport

import "os"

// Windows has no usblp; USB printing there goes through the spooler (a later
// phase). Keep the build green with the plain open.
func openBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY, 0)
}
