package discover

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/xyz/orderly-print-bridge/internal/api"
)

// USBGlob is where Linux puts USB printer-class device nodes (the `usblp`
// driver). A printer that enumerates as a printer-class device appears here; one
// that only speaks a vendor protocol does not, and no amount of scanning will
// find it.
const USBGlob = "/dev/usb/lp*"

// usbClassDirs are the sysfs class directories usblp registers under, newest
// kernels first. `/sys/class/usbmisc` is where lp0 lives on Debian 12 (the box);
// `/sys/class/usb` is the older location.
var usbClassDirs = []string{"/sys/class/usbmisc", "/sys/class/usb"}

// scanUSB lists the local USB printer nodes. It is Linux-only in this release:
// a macOS printer shows up as /dev/cu.* with no class metadata, and Windows USB
// printing goes through the spooler rather than a device node — both are
// deferred rather than half-guessed.
//
// Every metadata read is best-effort. A container with the device node passed
// through but no /sys still reports the node, because the node is the part that
// can actually be printed to.
func scanUSB(opts *Options) []api.DiscoveredPrinter {
	if opts.goos() != "linux" {
		return nil
	}
	root := opts.USBRoot
	nodes, err := filepath.Glob(root + USBGlob)
	if err != nil || len(nodes) == 0 {
		return nil
	}
	out := make([]api.DiscoveredPrinter, 0, len(nodes))
	for _, node := range nodes {
		info := readUSBMetadata(root, filepath.Base(node))
		out = append(out, api.DiscoveredPrinter{
			// The scheme is part of the address so a manager can paste this row
			// straight into the printer form (transport.Send accepts it).
			Address:    "usb:" + node,
			Transport:  api.TransportUSB,
			Model:      info.model(),
			VendorID:   info.vendor,
			ProductID:  info.product,
			LastSeenAt: opts.now(),
		})
	}
	return out
}

type usbInfo struct {
	vendor       string // idVendor, e.g. "0483"
	product      string // idProduct
	manufacturer string
	name         string // the sysfs `product` string
}

func (u usbInfo) model() string {
	label := strings.TrimSpace(u.manufacturer + " " + u.name)
	if label != "" {
		return label
	}
	if u.vendor != "" || u.product != "" {
		return strings.TrimSpace(u.vendor + ":" + u.product)
	}
	return ""
}

// readUSBMetadata reads the four sysfs attributes that make a device node
// recognisable to a human: `/sys/class/usbmisc/lp0/device/../idVendor` and its
// siblings.
//
// The paths are concatenated, NOT filepath.Join'd, on purpose: `device` is a
// symlink into the USB interface directory, and `..` from there is the USB
// DEVICE directory that holds the ids. filepath.Join would clean `..` away
// lexically and read the wrong directory.
func readUSBMetadata(root, node string) usbInfo {
	var info usbInfo
	for _, class := range usbClassDirs {
		base := root + class + "/" + node + "/device/.."
		if _, err := os.Stat(base); err != nil {
			continue
		}
		info.vendor = readAttr(base + "/idVendor")
		info.product = readAttr(base + "/idProduct")
		info.manufacturer = readAttr(base + "/manufacturer")
		info.name = readAttr(base + "/product")
		if info.vendor != "" || info.name != "" || info.manufacturer != "" {
			break
		}
	}
	return info
}

func readAttr(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// sysfs attributes are short and newline-terminated; anything longer than a
	// line is not one of these attributes.
	s := strings.TrimSpace(string(body))
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
