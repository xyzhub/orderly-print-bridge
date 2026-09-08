// Package discover answers one question for the manager page: what printers can
// this box SEE? (master-plan task 49.)
//
// It is deliberately small and deliberately powerless:
//
//   - A candidate is NEVER routing input. The daemon prints only to what the
//     server assigned it; this package's whole output is a list a human reads
//     when filling in that assignment. A device that could nominate its own
//     printer would be a device that can be talked into printing a venue's
//     invoices onto an attacker's spool.
//   - The network sweep is bounded on every axis — one /24 (never wider), 32
//     concurrent dials, a 300 ms dial timeout, ~10 s for the whole sweep, and
//     at most one sweep per SweepInterval. A discovery feature that saturates a
//     venue's switch during service is worse than no discovery feature.
//   - There is no mDNS (LD-32). `_pdl-datastream._tcp` needs a resolver
//     dependency and an open UDP port for a marginal gain over "who answers on
//     9100"; v1.1 does not take that on.
//
// Answering on 9100 is not proof of being a thermal printer — 9100 is
// JetDirect and an office LaserJet answers too — so each responder is asked
// `GS I 1` exactly the way the welcome-slip gate asks it. Silence is recorded
// as "not identified", never as "not a printer".
package discover

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/api"
	"github.com/xyz/orderly-print-bridge/internal/escpos"
	"github.com/xyz/orderly-print-bridge/internal/transport"
)

// The bounds. Every one of them is a promise to the venue's network.
const (
	// Port is the raw-print (JetDirect) port. It is the only port swept.
	Port = 9100
	// Concurrency caps simultaneous dials.
	Concurrency = 32
	// DialTimeout bounds one probe. A printer on the same LAN answers in
	// single-digit milliseconds; 300 ms is already generous.
	DialTimeout = 300 * time.Millisecond
	// Budget bounds the whole sweep, host enumeration included.
	Budget = 10 * time.Second
	// SweepInterval is the minimum gap between sweeps. The daemon also sweeps
	// on demand (SIGUSR1) so an installer never has to wait it out.
	SweepInterval = 10 * time.Minute
	// MaxHosts is the widest network this will ever walk: a /24 (254 usable
	// addresses) plus headroom. A box on a /16 sweeps the /24 around itself,
	// never the /16.
	MaxHosts = 512
)

// Options configures a sweep. The zero value is the production configuration.
type Options struct {
	// Interfaces overrides local address enumeration (tests).
	Interfaces func() ([]net.Addr, error)
	// Dial probes one TCP address; nil means a real net.DialTimeout.
	Dial func(ctx context.Context, address string) error
	// Probe asks a target for its identity; nil means transport.Query.
	Probe func(target string, cmd []byte) ([]byte, error)
	// USBRoot is the filesystem root the USB scan reads under (tests). Empty
	// means "/".
	USBRoot string
	// GOOS overrides the OS for the USB scan (tests). Empty means runtime.GOOS.
	GOOS string
	// Now sources timestamps; nil means time.Now.
	Now func() time.Time
	// Logf receives one summary line per sweep.
	Logf func(format string, args ...any)
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Sweep returns every printer candidate this box can see: the network probe
// first, then the local USB device nodes.
//
// It returns an EMPTY slice and a nil error when nothing answers — the common
// case on a venue's network, and not a failure. Only a sweep that could not
// start at all (no usable interface) returns an error, and even then the caller
// treats it as "nothing to report".
func Sweep(ctx context.Context, opts Options) ([]api.DiscoveredPrinter, error) {
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()

	found := make([]api.DiscoveredPrinter, 0, 8)
	network, err := sweepNetwork(ctx, &opts)
	found = append(found, network...)
	found = append(found, scanUSB(&opts)...)

	if len(found) > api.MaxDiscovered {
		found = found[:api.MaxDiscovered]
	}
	opts.logf("discovery sweep: %d candidate(s)", len(found))
	if err != nil && len(found) == 0 {
		return found, err
	}
	return found, nil
}

// sweepNetwork probes every host on this box's own /24 for TCP 9100.
func sweepNetwork(ctx context.Context, opts *Options) ([]api.DiscoveredPrinter, error) {
	hosts, err := LocalHosts(opts.Interfaces)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, nil
	}

	dial := opts.Dial
	if dial == nil {
		dial = dialTCP
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		hits []string
		sem  = make(chan struct{}, Concurrency)
	)
	for _, host := range hosts {
		select {
		case <-ctx.Done():
			// Out of budget: report what answered so far rather than nothing.
		default:
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-sem }()
			address := fmt.Sprintf("%s:%d", h, Port)
			if err := dial(ctx, address); err != nil {
				return
			}
			mu.Lock()
			hits = append(hits, address)
			mu.Unlock()
		}(host)
	}
	wg.Wait()

	// Identity is asked serially and only of the handful that answered: a
	// `GS I` to something that is not a printer gets no reply and costs a full
	// query timeout, so this must not be part of the fan-out.
	probe := opts.Probe
	if probe == nil {
		probe = transport.Query
	}
	out := make([]api.DiscoveredPrinter, 0, len(hits))
	for _, address := range hits {
		candidate := api.DiscoveredPrinter{
			Address:    address,
			Transport:  api.TransportTCP,
			LastSeenAt: opts.now(),
		}
		if reply, err := probe("tcp://"+address, escpos.CmdIdentity); err == nil && len(reply) > 0 {
			candidate.Identity = printable(reply)
			candidate.Model = candidate.Identity
		}
		out = append(out, candidate)
	}
	return out, nil
}

func dialTCP(ctx context.Context, address string) error {
	d := net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	// Close immediately: an open socket on 9100 that sends nothing still ties
	// up the printer's single session slot on cheap firmwares.
	return conn.Close()
}

// LocalHosts enumerates the addresses to probe: every host on the /24 of each
// usable IPv4 interface, minus this box's own address. Exported for the test
// and for `orderly-print-bridge discover`, which prints what it will sweep.
//
// A /23 or wider is narrowed to the /24 around our own address — a venue on a
// flat /16 must not become a 65,000-host scan.
func LocalHosts(interfaces func() ([]net.Addr, error)) ([]string, error) {
	if interfaces == nil {
		interfaces = net.InterfaceAddrs
	}
	addrs, err := interfaces()
	if err != nil {
		return nil, fmt.Errorf("discover: cannot read this host's addresses: %w", err)
	}
	seen := map[string]bool{}
	var hosts []string
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		base := ip.Mask(net.CIDRMask(24, 32))
		for i := 1; i < 255; i++ {
			candidate := net.IPv4(base[0], base[1], base[2], byte(i)).String()
			if candidate == ip.String() || seen[candidate] {
				continue
			}
			seen[candidate] = true
			hosts = append(hosts, candidate)
			if len(hosts) >= MaxHosts {
				return hosts, nil
			}
		}
	}
	return hosts, nil
}

// printable keeps a `GS I` reply readable in a JSON payload and on a web page:
// some models answer with a model-id byte, others with an ASCII name, and one
// answers with 0x00.
func printable(reply []byte) string {
	var b strings.Builder
	for _, c := range reply {
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "\\x%02x", c)
	}
	s := strings.TrimSpace(b.String())
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func (o *Options) goos() string {
	if o.GOOS != "" {
		return o.GOOS
	}
	return runtime.GOOS
}
