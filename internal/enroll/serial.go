package enroll

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// LinuxSerialPaths are the DMI nodes read, in order. Both are mode 0400
// root-only in the kernel (drivers/firmware/dmi-id.c), which is why the box
// container runs as root with /sys/class/dmi/id mounted read-only. A PC
// without root simply reads nothing and the setup code becomes the identity.
var LinuxSerialPaths = []string{
	"/sys/class/dmi/id/product_serial",
	"/sys/class/dmi/id/board_serial",
}

// placeholderSerials are the strings mainboard vendors ship instead of a
// serial. Treating them as identities would make every unconfigured machine of
// a given model collide on one `serial @unique` row, so they read as absent.
var placeholderSerials = map[string]bool{
	"":                         true,
	"none":                     true,
	"n/a":                      true,
	"na":                       true,
	"null":                     true,
	"0":                        true,
	"123456789":                true,
	"default string":           true,
	"system serial number":     true,
	"to be filled by o.e.m.":   true,
	"to be filled by o.e.m":    true,
	"not specified":            true,
	"not applicable":           true,
	"invalid":                  true,
	"xxxxxxx":                  true,
	"................":         true,
	"unknown":                  true,
	"chassis serial number":    true,
	"base board serial number": true,
	"empty":                    true,
}

type fileReader func(string) ([]byte, error)
type commandRunner func(name string, args ...string) ([]byte, error)

func runCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Serial reads the hardware serial for the running OS, or "" when it cannot be
// read. "" is a legitimate answer, never an error: the setup code is the
// identity in that case (master-plan task 22).
func Serial() string { return serialFor(runtime.GOOS, os.ReadFile, runCommand) }

func serialFor(goos string, read fileReader, run commandRunner) string {
	switch goos {
	case "linux":
		return serialLinux(read)
	case "darwin":
		return serialDarwin(run)
	case "windows":
		return serialWindows(run)
	default:
		return ""
	}
}

func serialLinux(read fileReader) string {
	for _, p := range LinuxSerialPaths {
		raw, err := read(p)
		if err != nil {
			continue
		}
		if s := cleanSerial(string(raw)); s != "" {
			return s
		}
	}
	return ""
}

// serialDarwin parses `ioreg -rd1 -c IOPlatformExpertDevice`, whose relevant
// line reads:  "IOPlatformSerialNumber" = "C02XXXXXXXXX"
func serialDarwin(run commandRunner) string {
	out, err := run("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "IOPlatformSerialNumber") {
			continue
		}
		i := strings.Index(line, "=")
		if i < 0 {
			continue
		}
		val := strings.TrimSpace(line[i+1:])
		val = strings.Trim(val, `"`)
		if s := cleanSerial(val); s != "" {
			return s
		}
	}
	return ""
}

// serialWindows asks WMI for Win32_BIOS.SerialNumber, preferring the CIM
// cmdlet and falling back to the deprecated wmic on older builds.
func serialWindows(run commandRunner) string {
	if out, err := run("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-CimInstance -ClassName Win32_BIOS).SerialNumber"); err == nil {
		if s := cleanSerial(string(out)); s != "" {
			return s
		}
	}
	if out, err := run("wmic", "bios", "get", "serialnumber"); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.EqualFold(line, "serialnumber") {
				continue
			}
			if s := cleanSerial(line); s != "" {
				return s
			}
		}
	}
	return ""
}

func cleanSerial(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\x00")
	s = strings.TrimSpace(s)
	if placeholderSerials[strings.ToLower(s)] {
		return ""
	}
	// A serial made only of punctuation/repeats is a placeholder too.
	if strings.Trim(s, ".-_ ") == "" {
		return ""
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}
