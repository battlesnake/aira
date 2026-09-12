//go:build linux

package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type chunkReader struct{ reader io.Reader }

func (r chunkReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.reader.Read(p)
}

type deadlineReadError struct{}

func (deadlineReadError) Error() string   { return "deadline" }
func (deadlineReadError) Timeout() bool   { return true }
func (deadlineReadError) Temporary() bool { return true }

type deadlineFrameConn struct {
	mu     sync.Mutex
	reader *bytes.Reader
	closed atomic.Bool
}

func (c *deadlineFrameConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.reader.Read(p)
	if n > 0 && c.reader.Len() == 0 {
		return n, deadlineReadError{}
	}
	return n, err
}
func (c *deadlineFrameConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *deadlineFrameConn) Close() error                { c.closed.Store(true); return nil }
func (c *deadlineFrameConn) LocalAddr() net.Addr         { return testNetAddr("local") }
func (c *deadlineFrameConn) RemoteAddr() net.Addr        { return testNetAddr("remote") }
func (c *deadlineFrameConn) SetDeadline(time.Time) error { return nil }
func (c *deadlineFrameConn) SetReadDeadline(time.Time) error {
	return nil
}
func (c *deadlineFrameConn) SetWriteDeadline(time.Time) error {
	return nil
}

type testNetAddr string

func (a testNetAddr) Network() string { return "test" }
func (a testNetAddr) String() string  { return string(a) }

type instantClock struct {
	mu  sync.Mutex
	now time.Time
}

func newInstantClock() *instantClock { return &instantClock{now: time.Unix(100, 0)} }
func (c *instantClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *instantClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

func currentSliceForTest(t *testing.T) string {
	t.Helper()
	mount, err := unifiedMount()
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	path, ok, reason := resolveSlicePathAt(current, mount, current)
	if !ok {
		t.Fatalf("resolve current slice: %s", reason)
	}
	return path
}

func gateOnlyRunner(t *testing.T, clock Clock, fn func(string) (int64, int64, bool, string)) (*Runner, string) {
	t.Helper()
	path := currentSliceForTest(t)
	return &Runner{memorySlice: path, memoryReserve: 40, admissionMaxWait: 100 * time.Millisecond, pollInterval: 10 * time.Millisecond, clock: clock, sliceMemory: fn}, path
}

func TestReadSliceMemoryTable(t *testing.T) {
	for _, tc := range []struct {
		name, current, max, reason string
		ok                         bool
		wantCurrent, wantMax       int64
		missing                    string
	}{
		{name: "values", current: "25\n", max: "100\n", ok: true, wantCurrent: 25, wantMax: 100},
		{name: "unbounded", current: "25", max: "max", reason: "unbounded"},
		{name: "empty current", current: "", max: "100", reason: "parse-error"},
		{name: "negative current", current: "-1", max: "100", reason: "parse-error"},
		{name: "nonnumeric max", current: "1", max: "many", reason: "parse-error"},
		{name: "overflow", current: "9223372036854775808", max: "100", reason: "parse-error"},
		{name: "missing current", max: "100", missing: "memory.current", reason: "read-error"},
		{name: "missing max", current: "1", missing: "memory.max", reason: "read-error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.missing != "memory.current" {
				if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte(tc.current), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missing != "memory.max" {
				if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(tc.max), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cur, max, ok, reason := readSliceMemory(dir)
			if ok != tc.ok || reason != tc.reason || (ok && (cur != tc.wantCurrent || max != tc.wantMax)) {
				t.Fatalf("read=(%d,%d,%v,%q)", cur, max, ok, reason)
			}
		})
	}

	t.Run("permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permission bits")
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("1"), 0); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("2"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, ok, reason := readSliceMemory(dir); ok || reason != "read-error" {
			t.Fatalf("ok=%v reason=%q", ok, reason)
		}
	})
}

func TestResolveSlicePathTable(t *testing.T) {
	mount := t.TempDir()
	ancestor := filepath.Join(mount, "user.slice", "whale.slice")
	current := filepath.Join(ancestor, "session.scope")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join(mount, "machine.slice")
	if err := os.Mkdir(relative, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(mount, "escape.slice")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value, want string
		ok                bool
	}{
		{name: "absolute", value: relative, want: relative, ok: true},
		{name: "mount relative", value: "machine.slice", want: relative, ok: true},
		{name: "bare ancestor", value: "whale.slice", want: ancestor, ok: true},
		{name: "nonexistent", value: "missing.slice"},
		{name: "dotdot escape", value: "../outside"},
		{name: "symlink escape", value: "escape.slice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, reason := resolveSlicePathAt(tc.value, mount, current)
			if ok != tc.ok || (!ok && reason != "slice-not-found") {
				t.Fatalf("resolve=(%q,%v,%q)", got, ok, reason)
			}
			if ok && filepath.Clean(got) != filepath.Clean(tc.want) {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestAdmissionT1DisabledAndNoDaemonUnevaluated(t *testing.T) {
	if result, err := (&Runner{}).admit(context.Background(), Request{}); err != nil || result.state != "disabled" {
		t.Fatalf("disabled result=%+v err=%v", result, err)
	}
	override := int64(70)
	if result, err := (&Runner{}).admit(context.Background(), Request{MemoryReserveOverride: &override}); err != nil || result.state != "disabled" {
		t.Fatalf("override enabled statically-disabled admission: result=%+v err=%v", result, err)
	}
	// S13: with a slice and reserve configured but NO admission daemon socket, admit()
	// no longer flock-gates (the fallback is deleted). A non-exclusive launch is
	// `unevaluated` with reason "no-daemon" and a stderr warning; it runs ungoverned
	// (and `--require-admission` refuses it). The injected sliceMemory fn is never
	// consulted — there is no self-gating loop left to read it.
	var diagnostics bytes.Buffer
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	r.diagnostics = &diagnostics
	result, err := r.admit(context.Background(), Request{})
	if err != nil || result.state != "unevaluated" || result.reason != "no-daemon" || !strings.Contains(diagnostics.String(), "warning") {
		t.Fatalf("no-daemon result=%+v err=%v diagnostics=%q", result, err, diagnostics.String())
	}
}

func TestRunnerNewDefaultsAndValidatesAdmissionTiming(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.admissionMaxWait != 30*time.Minute || r.pollInterval != 2*time.Second {
		t.Fatalf("maxWait=%s poll=%s", r.admissionMaxWait, r.pollInterval)
	}
	for _, cfg := range []Config{
		{CommonDir: t.TempDir(), AdmissionMaxWait: -time.Second},
		{CommonDir: t.TempDir(), PollInterval: -time.Second},
	} {
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
			t.Fatalf("New(%+v) err=%v", cfg, err)
		}
	}
}

type countingBackend struct {
	scope   *memoryScope
	creates atomic.Int64
}

func (b *countingBackend) Probe(context.Context) error { return nil }
func (b *countingBackend) Create(context.Context, string) (Scope, error) {
	b.creates.Add(1)
	return b.scope, nil
}
func (b *countingBackend) Open(context.Context, string) (Scope, error) { return b.scope, nil }

func TestAdmissionT6NoAdmitBypassesPressure(t *testing.T) {
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { panic("must not read") })
	result, err := r.admit(context.Background(), Request{NoAdmit: true})
	if err != nil || result.state != "bypassed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestAdmissionDiagnosticsErrorsAreIgnored(t *testing.T) {
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 0, false, "read-error" })
	r.diagnostics = failingWriter{}
	if result, err := r.admit(context.Background(), Request{}); err != nil || result.state != "unevaluated" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestDaemonAdmitReadFullFraming(t *testing.T) {
	want := runnerAdmitResponseFrame{OK: true, Code: "OK", Data: json.RawMessage(`{"state":"waited","waited_ms":17}`)}
	var encoded bytes.Buffer
	if err := writeRunnerAdmitFrame(&encoded, want); err != nil {
		t.Fatal(err)
	}
	var got runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(chunkReader{reader: bytes.NewReader(encoded.Bytes())}, &got); err != nil {
		t.Fatalf("one-byte framing failed: %v", err)
	}
	if !got.OK || got.Code != "OK" || !bytes.Equal(got.Data, want.Data) {
		t.Fatalf("frame=%+v", got)
	}
	for name, cut := range map[string]int{"partial header": 2, "partial payload": len(encoded.Bytes()) - 1} {
		t.Run(name, func(t *testing.T) {
			var frame runnerAdmitResponseFrame
			if err := readRunnerAdmitFrame(bytes.NewReader(encoded.Bytes()[:cut]), &frame); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error=%v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func TestDaemonAdmitGrantStatesRemainByteIdentical(t *testing.T) {
	for _, state := range []string{"immediate", "waited", "unevaluated"} {
		t.Run(state, func(t *testing.T) {
			clock := newInstantClock()
			runner, _ := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
			client, server := net.Pipe()
			runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
			go func() {
				defer server.Close()
				var request runnerAdmitRequestFrame
				if readRunnerAdmitFrame(server, &request) != nil {
					return
				}
				data, _ := json.Marshal(runnerAdmitGrant{State: state, Reason: "reason", WaitedMS: 7, Reserve: 40, Basis: "pinned:client"})
				_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
				var one [1]byte
				_, _ = server.Read(one[:])
			}()
			result, err := runner.admit(context.Background(), Request{})
			if err != nil || result.state != state || result.reason != "reason" || result.waitedMS != 7 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			result.releaseAdmission()
		})
	}
	for _, state := range []string{"disabled", "bypassed"} {
		runner := &Runner{clock: newInstantClock()}
		request := Request{NoAdmit: state == "bypassed"}
		if state == "bypassed" {
			runner.memorySlice, runner.memoryReserve = "/unused", 1
		}
		result, err := runner.admit(context.Background(), request)
		if err != nil || result.state != state {
			t.Fatalf("%s result=%+v err=%v", state, result, err)
		}
	}
}

func TestConfineAdmitFrameCarriesScopeNameAndOwner(t *testing.T) {
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	seen := make(chan map[string]any, 1)
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		seen <- request.Request.Args
		data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 40, Basis: "pinned:client"})
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	result, err := r.admit(context.Background(), Request{ConfineScopeID: "CONFINE-job-123-abc", ConfineName: "job", ConfineOwner: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	args := <-seen
	if args["scope_id"] != "CONFINE-job-123-abc" || args["name"] != "job" || args["owner"] != "session-a" {
		t.Fatalf("admit args=%v", args)
	}
	result.releaseAdmission()
}

func TestDaemonAdmitFrameUsesOverrideReserve(t *testing.T) {
	positive, zero, negative := int64(70), int64(0), int64(-1)
	for _, test := range []struct {
		name     string
		override *int64
		want     int64
	}{
		{name: "nil preserves static", want: 40},
		{name: "positive replaces static", override: &positive, want: positive},
		{name: "zero preserves static", override: &zero, want: 40},
		{name: "negative preserves static", override: &negative, want: 40},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
			client, server := net.Pipe()
			r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
			reserveCh := make(chan int64, 1)
			go func() {
				defer server.Close()
				var request runnerAdmitRequestFrame
				if err := readRunnerAdmitFrame(server, &request); err != nil {
					return
				}
				reserve, _ := request.Request.Args["reserve"].(float64)
				reserveCh <- int64(reserve)
				data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: test.want, Basis: "pinned:client"})
				_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
				var one [1]byte
				_, _ = server.Read(one[:])
			}()
			result, err := r.admit(context.Background(), Request{MemoryReserveOverride: test.override})
			if err != nil {
				t.Fatal(err)
			}
			defer result.releaseAdmission()
			if got := <-reserveCh; got != test.want {
				t.Fatalf("daemon reserve=%d want %d", got, test.want)
			}
		})
	}
}

func TestAdmissionProvenanceStampedOnForegroundRecords(t *testing.T) {
	path := currentSliceForTest(t)
	newRunner := func(t *testing.T, configured bool) *Runner {
		t.Helper()
		cfg := Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, Clock: newInstantClock()}
		if configured {
			cfg.MemorySlice, cfg.MemoryReserve = path, 40
			cfg.sliceMemoryFn = func(string) (int64, int64, bool, string) { return 0, 100, true, "" }
		}
		r, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, test := range []struct {
		name       string
		configured bool
		request    Request
		want       *int64
		basis      string
	}{
		{name: "override", configured: true, request: Request{Argv: []string{"/bin/true"}, ResourceSignature: "sig", MemoryReserveOverride: int64Pointer(70), MemoryReserveBasis: "estimate:max=60,n=3,f=115"}, want: int64Pointer(70), basis: "estimate:max=60,n=3,f=115"},
		{name: "static nil override", configured: true, request: Request{Argv: []string{"/bin/true"}, ResourceSignature: "sig"}, want: int64Pointer(40)},
		{name: "no admit", configured: true, request: Request{Argv: []string{"/bin/true"}, ResourceSignature: "sig", NoAdmit: true}, basis: "disabled:no-admit"},
		{name: "disabled config", request: Request{Argv: []string{"/bin/true"}, ResourceSignature: "sig"}, basis: "disabled:config"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newRunner(t, test.configured)
			r.startFn = func(*exec.Cmd) error { return errors.New("injected after record construction") }
			if _, err := r.Launch(context.Background(), test.request); err == nil {
				t.Fatal("injected launch failure was not returned")
			}
			record, err := r.Get("RUN-1")
			if err != nil {
				t.Fatal(err)
			}
			if record.ResourceSignature != "sig" || record.AdmissionReserveBasis != test.basis || !equalInt64Pointer(record.AdmissionReserve, test.want) {
				t.Fatalf("record signature=%q reserve=%v basis=%q", record.ResourceSignature, record.AdmissionReserve, record.AdmissionReserveBasis)
			}
			if err := r.ledger.project(context.Background()); err != nil {
				t.Fatal(err)
			}
			var signature, basis sql.NullString
			var reserve sql.NullInt64
			var recordJSON []byte
			db, err := sql.Open("sqlite", r.ledger.projection)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.QueryRow(`SELECT resource_signature,admission_reserve,admission_reserve_basis,record_json FROM runs WHERE id=?`, record.ID).Scan(&signature, &reserve, &basis, &recordJSON); err != nil {
				t.Fatal(err)
			}
			if !signature.Valid || signature.String != "sig" || reserve.Valid != (test.want != nil) || reserve.Valid && reserve.Int64 != *test.want || basis.Valid != (test.basis != "") || basis.Valid && basis.String != test.basis {
				t.Fatalf("columns signature=%v reserve=%v basis=%v", signature, reserve, basis)
			}
			var projected RunRecord
			if err := json.Unmarshal(recordJSON, &projected); err != nil {
				t.Fatal(err)
			}
			if projected.ResourceSignature != "sig" || projected.AdmissionReserveBasis != test.basis || !equalInt64Pointer(projected.AdmissionReserve, test.want) {
				t.Fatalf("record_json signature=%q reserve=%v basis=%q", projected.ResourceSignature, projected.AdmissionReserve, projected.AdmissionReserveBasis)
			}
		})
	}
}

func int64Pointer(value int64) *int64 { return &value }

func equalInt64Pointer(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func TestDaemonAdmitStatesAreByteIdenticalOnRunRecord(t *testing.T) {
	for _, state := range []string{"immediate", "waited", "unevaluated"} {
		t.Run(state, func(t *testing.T) {
			path := currentSliceForTest(t)
			runner, err := New(Config{
				CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}},
				MemorySlice: path, MemoryReserve: 40, AdmissionMaxWait: time.Second,
				PollInterval: time.Millisecond, Clock: newInstantClock(),
				sliceMemoryFn: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
			})
			if err != nil {
				t.Fatal(err)
			}
			client, server := net.Pipe()
			runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
			go func() {
				defer server.Close()
				var request runnerAdmitRequestFrame
				if readRunnerAdmitFrame(server, &request) != nil {
					return
				}
				data, _ := json.Marshal(runnerAdmitGrant{State: state, Reason: "wire-reason", WaitedMS: 11, Reserve: 40, Basis: "pinned:client"})
				_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
				var one [1]byte
				_, _ = server.Read(one[:])
			}()
			runner.startFn = func(*exec.Cmd) error { return errors.New("injected after admission") }
			if _, err := runner.Launch(context.Background(), Request{Argv: []string{"/bin/true"}}); err == nil {
				t.Fatal("injected Start error not returned")
			}
			events, err := runner.ledger.read()
			if err != nil || len(events) == 0 {
				t.Fatalf("events=%d err=%v", len(events), err)
			}
			record := events[len(events)-1].Run
			if record.Admission != state || record.AdmissionReason != "wire-reason" || record.AdmissionWaitedMS != 11 {
				t.Fatalf("record admission=(%q,%q,%d)", record.Admission, record.AdmissionReason, record.AdmissionWaitedMS)
			}
		})
	}
}

// S13 replaced the daemon-failure→flock fallback with reconnect (transport
// failure) or a terminal refuse (well-formed refusal). The former
// TestDaemonAdmitWedgedDaemonDeadlineFallsToFlock and
// TestDaemonAdmitPartialFrameFallsToFlock pinned the removed behavior (a wedged
// daemon tripping a transport deadline into flock; a partial frame into flock).
// The new behavior — block/reconnect on transport failure, never fall open — is
// pinned by TestAdmitTransportFailureReconnectsAndNeverFlocks,
// TestAdmitWellFormedRefusalIsTerminalNeverFlocks, and
// TestAdmitWedgedDaemonBlocksUntilContextCancel below.

func TestDaemonAdmitFullFrameWinsWithoutFlock(t *testing.T) {
	data, err := json.Marshal(runnerAdmitGrant{State: "waited", WaitedMS: 9, Reserve: 40, Basis: "pinned:client"})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := writeRunnerAdmitFrame(&encoded, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data}); err != nil {
		t.Fatal(err)
	}
	conn := &deadlineFrameConn{reader: bytes.NewReader(encoded.Bytes())}
	path := currentSliceForTest(t)
	runner := &Runner{
		memorySlice: path, memoryReserve: 40, admissionMaxWait: time.Millisecond,
		pollInterval: time.Millisecond, clock: newInstantClock(),
		sliceMemory: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
		admitDialFn: func(context.Context, string) (net.Conn, error) { return conn, nil },
	}
	// S13: there is no flock fallback to probe — "never flocks" is now structural.
	// A full validated grant wins and its connection becomes the lease.
	result, err := runner.admit(context.Background(), Request{})
	if err != nil || result.state != "waited" || result.waitedMS != 9 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if conn.closed.Load() {
		t.Fatal("winning grant connection closed before launch release")
	}
	result.releaseAdmission()
	if !conn.closed.Load() {
		t.Fatal("winning grant connection was not released")
	}
}

func TestAdmitTransportFailureReconnectsAndNeverFlocks(t *testing.T) {
	// S13: a TRANSPORT failure (dial refused/ENOENT, or the daemon EOF'd mid-exchange)
	// RECONNECTS at 2/sec until the daemon answers — it NEVER falls open to the flock
	// fallback (the AIRA-222 class this slice closes). Here two dials fail (the daemon is
	// restarting) and the third is answered with a grant.
	path := currentSliceForTest(t)
	var dials atomic.Int64
	dial := func(context.Context, string) (net.Conn, error) {
		if dials.Add(1) <= 2 {
			return nil, os.ErrNotExist
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var request runnerAdmitRequestFrame
			if readRunnerAdmitFrame(server, &request) != nil {
				return
			}
			data, _ := json.Marshal(runnerAdmitGrant{State: "waited", WaitedMS: 3, Reserve: 40, Basis: "pinned:client"})
			_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
			var one [1]byte
			_, _ = server.Read(one[:]) // hold the lease until the client closes
		}()
		return client, nil
	}
	runner := &Runner{
		memorySlice: path, memoryReserve: 40, admissionMaxWait: time.Second,
		pollInterval: time.Millisecond, clock: newInstantClock(), admitDialFn: dial,
		sliceMemory: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
	}
	result, err := runner.admit(context.Background(), Request{})
	if err != nil || result.state != "waited" {
		t.Fatalf("result=%+v err=%v (want a reconnect grant)", result, err)
	}
	if dials.Load() != 3 {
		t.Fatalf("dials=%d, want 3 (two transport failures, then a grant)", dials.Load())
	}
	result.releaseAdmission()
}

func TestAdmitWellFormedRefusalIsTerminalNeverFlocks(t *testing.T) {
	// S13: a WELL-FORMED refusal frame — a fail-closed E_DAEMON_UNAVAILABLE, a
	// transiently-overloaded E_DAEMON_BUSY, or a version-skew E_DAEMON_PROTOCOL — is a
	// genuine daemon "no", not a transport failure. The client refuses to launch
	// (terminal error) rather than reconnecting on it or falling open to the flock
	// fallback. This is exactly the ungoverned-launch class the removed fallback caused.
	for _, code := range []string{"E_DAEMON_BUSY", "E_DAEMON_PROTOCOL", "E_DAEMON_UNAVAILABLE"} {
		t.Run(code, func(t *testing.T) {
			path := currentSliceForTest(t)
			dial := func(context.Context, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() {
					defer server.Close()
					var request runnerAdmitRequestFrame
					_ = readRunnerAdmitFrame(server, &request)
					_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: code, Error: "refused"})
				}()
				return client, nil
			}
			runner := &Runner{
				memorySlice: path, memoryReserve: 40, admissionMaxWait: time.Second,
				pollInterval: time.Millisecond, clock: newInstantClock(), admitDialFn: dial,
				sliceMemory: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
			}
			result, err := runner.admit(context.Background(), Request{})
			if err == nil {
				t.Fatalf("result=%+v err=nil; a well-formed %s refusal must be a terminal error, never a launch", result, code)
			}
		})
	}
}

func TestAdmitWedgedDaemonBlocksUntilContextCancel(t *testing.T) {
	// S13: a wedged daemon (dial succeeds, no reply) has NO client transport deadline —
	// the client blocks until the caller cancels (design §6: close the connection to
	// cancel). It never self-times-out into the flock fallback.
	path := currentSliceForTest(t)
	dial := func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		go func() {
			var request runnerAdmitRequestFrame
			_ = readRunnerAdmitFrame(server, &request) // read the request, then never reply
			var one [1]byte
			_, _ = server.Read(one[:])
		}()
		return client, nil
	}
	runner := &Runner{
		memorySlice: path, memoryReserve: 40, admissionMaxWait: time.Second,
		pollInterval: time.Millisecond, clock: systemClock{}, admitDialFn: dial,
		sliceMemory: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := runner.admit(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled (the client blocks on a wedged daemon until cancelled)", err)
	}
}

func TestRunSaturatedAdmissionIsTerminalBeforeScopeOrLedger(t *testing.T) {
	path := currentSliceForTest(t)
	backend := &countingBackend{scope: &memoryScope{}}
	r, err := New(Config{
		CommonDir: t.TempDir(), Backend: backend, MemorySlice: path, MemoryReserve: 40,
		AdmissionMaxWait: time.Second, PollInterval: time.Millisecond, Clock: newInstantClock(),
		sliceMemoryFn: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		_ = readRunnerAdmitFrame(server, &request)
		data, _ := json.Marshal(runnerAdmitRejection{Basis: "reject:saturated"})
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: "E_ADMIT_SATURATED", Error: "E_ADMIT_SATURATED: capacity remained full", Data: data})
	}()
	if record, err := r.Launch(context.Background(), Request{Argv: []string{"must-not-run"}}); err == nil || !strings.Contains(err.Error(), "E_ADMIT_SATURATED") || record != nil {
		t.Errorf("record=%+v err=%v", record, err)
	}
	if backend.creates.Load() != 0 {
		t.Fatalf("terminal run rejection created %d scopes", backend.creates.Load())
	}
	if events, err := r.ledger.read(); err != nil || len(events) != 0 {
		t.Fatalf("terminal run rejection events=%d err=%v", len(events), err)
	}
}
