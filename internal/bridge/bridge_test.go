package bridge

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/api"
	"github.com/xyz/orderly-print-bridge/internal/api/apitest"
	"github.com/xyz/orderly-print-bridge/internal/config"
	"github.com/xyz/orderly-print-bridge/internal/escpos"
	"github.com/xyz/orderly-print-bridge/internal/secret"
	"github.com/xyz/orderly-print-bridge/internal/transport"
)

// makePNG renders a receipt-shaped test artifact: white paper with a solid
// black block on the left half, so the round-trip can prove ink landed where
// it was drawn rather than merely that bytes moved.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for y := 4; y < h-4; y++ {
		for x := 4; x < w/2; x++ {
			img.SetGray(x, y, color.Gray{Y: 0})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func newBridge(t *testing.T, srv *apitest.Server, printers ...api.Printer) *Bridge {
	t.Helper()
	cfg := &config.Config{
		ServerURL: srv.URL(),
		Token:     secret.Secret(srv.Token),
		DeviceID:  srv.DeviceID,
		VenueID:   srv.VenueID,
		Printers:  printers,
	}
	cfg.SetPath(filepath.Join(t.TempDir(), "bridge.json"))
	b := &Bridge{
		Client:            api.New(cfg.ServerURL, cfg.Token),
		Cfg:               cfg,
		Send:              transport.Send,
		Probe:             transport.Query,
		PollInterval:      10 * time.Millisecond,
		HeartbeatInterval: time.Hour, // one heartbeat per test unless asked
		AckTimeout:        DefaultAckTimeout,
		Logf:              t.Logf,
	}
	srv.SetPrinters(printers...)
	return b
}

func fileSink(t *testing.T, widthDots int) (api.Printer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sink.escpos")
	return api.Printer{
		ID: "prn_sink", Name: "Sink", Transport: "file", Address: path,
		WidthDots: widthDots, DPI: 180, BandHeight: 128, Threshold: 128,
	}, path
}

// The end-to-end acceptance for master-plan task 23: an httptest server drives
// the whole loop with a file:// sink whose bytes pass the --decode round trip.
func TestEndToEndPollPrintAckRoundTrips(t *testing.T) {
	const width = 512 // client #1's SRP-E300 at 180 dpi
	srv := apitest.New(t)
	printer, sinkPath := fileSink(t, width)
	b := newBridge(t, srv, printer)

	artifact := makePNG(t, width, 240)
	srv.Enqueue(api.Job{ID: "job_1", Kind: api.KindReceipt, Printer: &printer, Dots: width}, artifact)

	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	acks := srv.Acks()
	if len(acks) != 1 {
		t.Fatalf("want exactly one ack, got %d: %+v", len(acks), acks)
	}
	if acks[0].JobID != "job_1" || acks[0].Body.Status != api.StatusPrinted {
		t.Fatalf("wrong ack: %+v", acks[0])
	}
	if acks[0].Body.BytesWritten <= 0 {
		t.Fatalf("the ack must report the bytes that reached the printer, got %d", acks[0].Body.BytesWritten)
	}
	if acks[0].Body.PrinterID != printer.ID {
		t.Fatalf("the ack must name the printer, got %q", acks[0].Body.PrinterID)
	}

	// --decode round trip: the sink's ESC/POS bytes decode back to an image of
	// exactly the printer's width, with the ink where it was drawn.
	raw, err := os.ReadFile(sinkPath)
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	if len(raw) != acks[0].Body.BytesWritten {
		t.Fatalf("the sink holds %d bytes but the ack reported %d", len(raw), acks[0].Body.BytesWritten)
	}
	decoded, err := escpos.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := decoded.Bounds().Dx(); got != width {
		t.Fatalf("decoded raster is %d dots wide, want %d", got, width)
	}
	if decoded.Bounds().Dy() != 240 {
		t.Fatalf("decoded raster is %d rows tall, want 240", decoded.Bounds().Dy())
	}
	if r, _, _, _ := decoded.At(20, 20).RGBA(); r != 0 {
		t.Errorf("expected ink at (20,20), got %v", decoded.At(20, 20))
	}
	if r, _, _, _ := decoded.At(width-20, 20).RGBA(); r == 0 {
		t.Errorf("expected blank paper at (%d,20)", width-20)
	}
}

// Counsel finding 4. A stall after bytes have reached the head is TERMINAL:
// re-queueing it is how one order becomes two legal invoices.
func TestPartialWriteIsTerminalAndNeverRequeued(t *testing.T) {
	const width = 576
	srv := apitest.New(t)
	printer, _ := fileSink(t, width)
	b := newBridge(t, srv, printer)

	sends := 0
	b.Send = func(string, []byte) (int, error) {
		sends++
		return 137, errors.New("write to printer 192.168.1.50:9100 (137 of 4096 bytes sent): connection reset by peer")
	}

	srv.Enqueue(api.Job{ID: "job_partial", Kind: api.KindReceipt, Printer: &printer, Dots: width},
		makePNG(t, width, 120))

	ctx := context.Background()
	if err := b.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// A second cycle: the daemon must not re-attempt the job on its own.
	if err := b.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	acks := srv.Acks()
	if len(acks) != 1 {
		t.Fatalf("want exactly one ack, got %d: %+v", len(acks), acks)
	}
	got := acks[0].Body
	if got.Status != api.StatusFailed {
		t.Fatalf("want status failed, got %q", got.Status)
	}
	if got.FailureReason != api.FailPartial {
		t.Fatalf("want failureReason %q, got %q", api.FailPartial, got.FailureReason)
	}
	if got.Retryable {
		t.Fatal("a partial write must NEVER be retryable — that is a second invoice")
	}
	if got.BytesWritten != 137 {
		t.Fatalf("the ack must carry the byte count that makes it terminal, got %d", got.BytesWritten)
	}
	if sends != 1 {
		t.Fatalf("the daemon retried locally %d times; it must not retry at all", sends)
	}
	_, _, fetches := srv.Counts()
	if fetches["job_partial"] != 1 {
		t.Fatalf("the artifact was fetched %d times; a partial write must not be re-attempted", fetches["job_partial"])
	}
}

// A zero-byte failure is the ONLY retryable class: nothing reached the paper.
func TestZeroByteFailureIsRetryable(t *testing.T) {
	const width = 576
	srv := apitest.New(t)
	printer, _ := fileSink(t, width)
	b := newBridge(t, srv, printer)
	b.Send = func(string, []byte) (int, error) {
		return 0, errors.New("connect to printer 192.168.1.50:9100: connection refused")
	}
	srv.Enqueue(api.Job{ID: "job_off", Kind: api.KindReceipt, Printer: &printer, Dots: width},
		makePNG(t, width, 120))

	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	acks := srv.Acks()
	if len(acks) != 1 {
		t.Fatalf("want one ack, got %d", len(acks))
	}
	if acks[0].Body.FailureReason != api.FailUnreachable || !acks[0].Body.Retryable {
		t.Fatalf("a zero-byte failure must be retryable{unreachable}, got %+v", acks[0].Body)
	}
}

// Master-plan task 23: a 576-wide PNG at a 512-dot printer acks
// failed{render_failed} and prints NOTHING. Never resample.
func TestWidthMismatchAcksRenderFailedAndNeverResamples(t *testing.T) {
	srv := apitest.New(t)
	printer, sinkPath := fileSink(t, 512)
	b := newBridge(t, srv, printer)

	sends := 0
	b.Send = func(target string, data []byte) (int, error) {
		sends++
		return transport.Send(target, data)
	}

	srv.Enqueue(api.Job{ID: "job_wide", Kind: api.KindReceipt, Printer: &printer, Dots: 512},
		makePNG(t, 576, 200)) // the render came back at the wrong width

	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	acks := srv.Acks()
	if len(acks) != 1 {
		t.Fatalf("want one ack, got %d", len(acks))
	}
	if acks[0].Body.Status != api.StatusFailed || acks[0].Body.FailureReason != api.FailRenderFailed {
		t.Fatalf("want failed{render_failed}, got %+v", acks[0].Body)
	}
	if acks[0].Body.Retryable {
		t.Fatal("a render at the wrong width will not fix itself on a retry")
	}
	if sends != 0 {
		t.Fatalf("bytes were sent to the printer %d time(s); a mismatched render must print nothing", sends)
	}
	if _, err := os.Stat(sinkPath); !os.IsNotExist(err) {
		t.Fatal("the sink was written; the artifact must never be resampled onto the wrong head")
	}
}

// Master-plan task 24: zero printers is a normal state — the heartbeat says
// printersDiscovered: 0 and nothing crashes.
func TestZeroPrintersHeartbeatsZeroAndDoesNotCrash(t *testing.T) {
	srv := apitest.New(t)
	b := newBridge(t, srv) // no printers at all
	b.HeartbeatInterval = time.Nanosecond

	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("tick with no printers: %v", err)
	}
	hbs := srv.Heartbeats()
	if len(hbs) == 0 {
		t.Fatal("no heartbeat was sent")
	}
	if hbs[0].PrintersDiscovered != 0 {
		t.Fatalf("printersDiscovered = %d, want 0", hbs[0].PrintersDiscovered)
	}
	if hbs[0].Version == "" {
		t.Fatal("the heartbeat must report the agent version")
	}
	if hbs[0].Discovered != nil {
		t.Fatal("v1 sends no discovery payload (Phase 4)")
	}

	// And a welcome slip in that state fails cleanly instead of printing blind.
	srv.Enqueue(api.Job{ID: "job_welcome", Kind: api.KindWelcome}, makePNG(t, 384, 100))
	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("welcome tick: %v", err)
	}
	acks := srv.Acks()
	if len(acks) != 1 || acks[0].Body.FailureReason != api.FailNoPrinter {
		t.Fatalf("want failed{no_printer}, got %+v", acks)
	}
}

// Counsel finding 6: port 9100 is JetDirect, so "the first printer found" can
// be an office LaserJet. A slip goes out only to an unambiguous candidate.
func TestWelcomeTargetIsNeverChosenBlind(t *testing.T) {
	counter := api.Printer{ID: "a", Name: "Counter", Transport: api.TransportTCP, Address: "10.0.0.5:9100", WidthDots: 512}
	laser := api.Printer{ID: "b", Name: "Office LaserJet", Transport: api.TransportTCP, Address: "10.0.0.9:9100", WidthDots: 512}
	silent := func(string, []byte) ([]byte, error) { return nil, nil }
	answers := func(only string) Prober {
		return func(target string, cmd []byte) ([]byte, error) {
			if !bytes.Equal(cmd, escpos.CmdIdentity) {
				t.Fatalf("the identity query must be GS I, got % x", cmd)
			}
			if strings.Contains(target, only) {
				return []byte{0x1d, 'S', 'R', 'P'}, nil
			}
			return nil, nil
		}
	}

	cases := []struct {
		name       string
		candidates []api.Printer
		probe      Prober
		want       string // printer ID, or "" for "do not print"
	}{
		{"exactly one candidate prints", []api.Printer{counter}, silent, "a"},
		{"zero candidates never print", nil, silent, ""},
		{"two candidates, none identifies: never print", []api.Printer{counter, laser}, silent, ""},
		{"two candidates, one answers GS I", []api.Printer{counter, laser}, answers("10.0.0.5"), "a"},
		{"two candidates, both answer: still ambiguous", []api.Printer{counter, laser},
			func(string, []byte) ([]byte, error) { return []byte{0x1d}, nil }, ""},
		{"probe errors are not identifications", []api.Printer{counter, laser},
			func(string, []byte) ([]byte, error) { return nil, errors.New("i/o timeout") }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := selectWelcomeTarget(tc.candidates, tc.probe, func(string, ...any) {})
			if tc.want == "" {
				if got != nil {
					t.Fatalf("printed blind to %s", got.Name)
				}
				if reason == "" {
					t.Fatal("a refusal must carry a reason the manager page can show")
				}
				return
			}
			if got == nil {
				t.Fatalf("expected printer %s, got none (%s)", tc.want, reason)
			}
			if got.ID != tc.want {
				t.Fatalf("chose %s, want %s", got.ID, tc.want)
			}
		})
	}
}

// A welcome job the server did not bind to a printer prints on the single
// configured candidate — and the decoded slip is exactly widthDots wide.
func TestWelcomeSlipPrintsOnTheSoleCandidateAtItsWidth(t *testing.T) {
	const width = 384 // counsel finding 5: the welcome slip is laid out at 384
	srv := apitest.New(t)
	printer, sinkPath := fileSink(t, width)
	b := newBridge(t, srv, printer)

	srv.Enqueue(api.Job{ID: "job_welcome", Kind: api.KindWelcome, Dots: width}, makePNG(t, width, 300))
	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	acks := srv.Acks()
	if len(acks) != 1 || acks[0].Body.Status != api.StatusPrinted {
		t.Fatalf("want a printed ack, got %+v", acks)
	}
	raw, err := os.ReadFile(sinkPath)
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	decoded, err := escpos.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := decoded.Bounds().Dx(); got != width {
		t.Fatalf("the welcome slip decoded %d dots wide, want %d", got, width)
	}
}

// The server naming a printer is a deliberate assignment and overrides the
// candidate gate — the gate only exists for the case where it named none.
func TestServerNamedPrinterWins(t *testing.T) {
	srv := apitest.New(t)
	named, _ := fileSink(t, 512)
	other := api.Printer{ID: "prn_other", Name: "Other", Transport: api.TransportTCP, Address: "10.0.0.9:9100", WidthDots: 512}
	b := newBridge(t, srv, other, named)

	job := &api.Job{ID: "j", Kind: api.KindWelcome, Printer: &named, Dots: 512}
	got, reason := b.selectPrinter(job)
	if got == nil || got.ID != named.ID {
		t.Fatalf("want the server-named printer, got %v (%s)", got, reason)
	}
}

// The 60 s budget: a wedged printer is acked failed{timeout}, terminal,
// because whether paper moved is unknown.
func TestJobThatOverrunsTheAckWindowAcksTimeout(t *testing.T) {
	srv := apitest.New(t)
	printer, _ := fileSink(t, 512)
	b := newBridge(t, srv, printer)
	b.AckTimeout = 50 * time.Millisecond
	b.Send = func(string, []byte) (int, error) {
		time.Sleep(2 * time.Second)
		return 0, nil
	}
	srv.Enqueue(api.Job{ID: "job_slow", Kind: api.KindReceipt, Printer: &printer, Dots: 512}, makePNG(t, 512, 100))

	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	acks := srv.Acks()
	if len(acks) != 1 {
		t.Fatalf("want one ack, got %d", len(acks))
	}
	if acks[0].Body.FailureReason != api.FailTimeout || acks[0].Body.Retryable {
		t.Fatalf("want a terminal failed{timeout}, got %+v", acks[0].Body)
	}
}

// A 410 on the artifact means the job was voided (the cashier printed in the
// browser) or expired. Nothing prints; the outcome is terminal.
func TestVoidedArtifactAcksTerminal(t *testing.T) {
	srv := apitest.New(t)
	printer, sinkPath := fileSink(t, 512)
	b := newBridge(t, srv, printer)
	srv.Enqueue(api.Job{ID: "job_void", Kind: api.KindReceipt, Printer: &printer, Dots: 512}, nil)
	srv.ArtifactStatus["job_void"] = 410

	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	acks := srv.Acks()
	if len(acks) != 1 || acks[0].Body.FailureReason != api.FailArtifactGone || acks[0].Body.Retryable {
		t.Fatalf("want a terminal failed{artifact_gone}, got %+v", acks)
	}
	if _, err := os.Stat(sinkPath); !os.IsNotExist(err) {
		t.Fatal("a voided job must not reach the printer")
	}
}

// A revoked device stops polling instead of hammering the rate limiter.
func TestRevokedTokenStopsTheLoop(t *testing.T) {
	srv := apitest.New(t)
	printer, _ := fileSink(t, 512)
	b := newBridge(t, srv, printer)
	b.Client = api.New(srv.URL(), secret.Secret("odb_wrong_token"))
	b.HeartbeatInterval = time.Nanosecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := b.Run(ctx)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("the operator word must be in the message: %v", err)
	}
}

// An assignment change from the heartbeat is persisted, so a restart with no
// network still knows where the backlog prints.
func TestHeartbeatPersistsThePrinterAssignment(t *testing.T) {
	srv := apitest.New(t)
	b := newBridge(t, srv)
	b.HeartbeatInterval = time.Nanosecond
	printer, _ := fileSink(t, 512)
	srv.SetPrinters(printer)

	if err := b.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(b.Cfg.Printers) != 1 || b.Cfg.Printers[0].ID != printer.ID {
		t.Fatalf("the assignment was not applied: %+v", b.Cfg.Printers)
	}
	reloaded, err := config.LoadFrom(b.Cfg.Path())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Printers) != 1 || reloaded.Printers[0].WidthDots != 512 {
		t.Fatalf("the assignment was not persisted: %+v", reloaded.Printers)
	}
	if b.PrintersDiscovered() != 1 {
		t.Fatalf("printersDiscovered = %d, want 1", b.PrintersDiscovered())
	}
}

func TestPrinterTargetURIs(t *testing.T) {
	cases := map[api.Printer]string{
		{Transport: api.TransportTCP, Address: "192.168.1.50:9100"}: "tcp://192.168.1.50:9100",
		{Transport: api.TransportUSB, Address: "/dev/usb/lp0"}:      "usb:///dev/usb/lp0",
		{Transport: "", Address: "/tmp/sink"}:                       "/tmp/sink",
	}
	for p, want := range cases {
		if got := p.Target(); got != want {
			t.Errorf("Target() = %q, want %q", got, want)
		}
	}
}
