//go:build linux

package runner

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type admissionResult struct {
	state        string
	reason       string
	waitedMS     int64
	release      io.Closer
	reserve      int64
	ceiling      int64
	scopeCeiling int64
	basis        string
}

var errDetachKillIntent = errors.New("detached run has a pending kill intent")

// errAdmitReconnect is an INTERNAL sentinel: admitExchangeOnce returns it when the
// admission exchange failed for a TRANSPORT reason (dial refused/ENOENT, or the
// daemon EOF'd mid-exchange) on a NON-exclusive request. admitThroughDaemon catches
// it and reconnects at 2/sec with no total deadline — the client fails CLOSED by
// waiting, never falling open to an ungoverned launch (design §4/§6; the AIRA-222
// class this slice closes). It never escapes admitThroughDaemon.
var errAdmitReconnect = errors.New("aira: admission transport failed; reconnect")

func (result admissionResult) releaseAdmission() {
	if result.release != nil {
		_ = result.release.Close()
	}
}

// DaemonProtocolVersion is the wire protocol the runner's admission client
// speaks. It MUST equal daemon.ProtocolVersion. The runner cannot import
// internal/daemon (daemon imports runner), so the two are pinned equal by
// TestRunnerDaemonProtocolVersionMatchesTheDaemon in the external runner_test
// package rather than derived — a bump on one side alone fails that test
// instead of silently breaking admission negotiation (AIRA-83 item 3).
//
// Bumped 9→10 for the admission-counter rebuild (S5 `cpu` admit arg + S7 signed
// ledger / version-frozen re-declare frame), then 10→11 in LOCKSTEP with
// daemon.ProtocolVersion for S15's worker-admit wire change (response gained
// parent_scope_id / available_bytes / available_cpu; max_wait_ms present-and-zero
// became a non-blocking snapshot). TestRunnerDaemonProtocolVersionMatchesTheDaemon
// fails if the two drift.
const DaemonProtocolVersion = 11

const (
	runnerDaemonMaxFrameBytes = 16 << 20
	admitTransportGrace       = time.Second
)

// errWorkerAdmitFrameSize is a SENTINEL, not a fresh errors.New at each call
// site, so RequestWorkerAdmit can classify "the daemon sent something that is
// not a frame" with errors.Is instead of matching the message text — the
// AIRA-42 class this channel exists to close.
var errWorkerAdmitFrameSize = errors.New("invalid daemon admission frame size")

type runnerAdmitRequestFrame struct {
	Proto   int            `json:"proto"`
	Scope   map[string]any `json:"scope"`
	Request struct {
		Verb       string         `json:"verb"`
		Args       map[string]any `json:"args,omitempty"`
		HasContent bool           `json:"has_content"`
	} `json:"request"`
}

type runnerAdmitResponseFrame struct {
	OK   bool   `json:"ok"`
	Code string `json:"code"`
	// Proto is set ONLY by the daemon's protocolMismatchFrame
	// (internal/daemon/protocol.go) — errorFrame and responseFrame both
	// leave it zero. That makes a non-zero Proto a STRUCTURAL discriminator
	// for "your client and this daemon speak different protocol versions",
	// which is how the worker-admit client tells a version skew apart from
	// an ordinary E_DAEMON_PROTOCOL argument rejection instead of matching
	// the words "daemon protocol is" out of the error sentence (AIRA-45,
	// AIRA-83(b)). Decoding it is what makes that possible; before this it
	// was simply dropped.
	Proto int             `json:"proto,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

type runnerAdmitGrant struct {
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	WaitedMS int64  `json:"waited_ms"`
	Reserve  int64  `json:"reserve"`
	// S5. Cpu echoes the granted CPU-core reservation, so the grant wire mirrors the
	// {ram, cpu} request vector. Informational only — the client applies no cpu.max —
	// and NOT part of validRunnerAdmitGrant: a 0-core grant (a delegate suite; §8) is
	// legal, so a zero here must never be read as an invalid grant.
	Cpu          int64  `json:"cpu,omitempty"`
	Basis        string `json:"basis"`
	ScopeCeiling int64  `json:"scope_ceiling,omitempty"`
}

type runnerAdmitRejection struct {
	Required int64  `json:"required,omitempty"`
	Ceiling  int64  `json:"cap_minus_headroom,omitempty"`
	Basis    string `json:"basis"`
	// AIRA-101. "draining" or "held" when the wait expired under slice
	// exclusivity, empty otherwise.
	//
	// Without this field the daemon's reason is dropped on unmarshal and the
	// operator's TERMINAL message still gives a memory diagnosis — "slice
	// contended, no memory admission within the wait" — for what was actually a
	// benchmark holding the slice. That sends them looking at RAM for a problem
	// that has nothing to do with RAM, and it would have made the daemon-side
	// reason inert at the one surface it exists for (found by build review).
	Exclusive string `json:"exclusive,omitempty"`

	// AIRA-149. The daemon's LATCHED diagnosis of the wait, mirrored here for the
	// same reason Exclusive is: a field the client does not unmarshal is a field
	// the operator never sees, which makes the daemon-side reading inert at the
	// one surface it exists for.
	//
	// Contention is "observed" / "none-observed" / "unevaluated". EMPTY is a
	// fourth, distinct state — "not reported by this daemon build" — and lands on
	// the unchanged pre-AIRA-149 wording; it must never be read as an established
	// solitude.
	//
	// Grantable is a POINTER because a measured zero and an absent field are
	// different facts: FormatConfineBytes(0) renders "unknown", this codebase's
	// word for NOT ESTABLISHED, so a measured zero has to be rendered separately
	// or the honest reading "not one byte was grantable" becomes "the daemon does
	// not know".
	Contention string `json:"contention,omitempty"`
	Grantable  *int64 `json:"grantable_bytes,omitempty"`
}

// ErrExclusiveUnavailable prefixes every refusal of an `--exclusive` request
// that could not be granted by the daemon (AIRA-101).
//
// The rule it enforces: an exclusive request NEVER degrades. Not to the flock
// fallback, not to an `unevaluated` launch, not to `disabled` or `bypassed`.
// This is the highest-priority property of the feature, and it comes from a real
// incident — an hour of benchmark throughput numbers was invalidated by
// contention nobody noticed. A benchmark that silently runs non-exclusively
// produces numbers that LOOK clean, which is strictly worse than no feature at
// all, so the only safe answer when exclusivity cannot be established is to
// refuse to launch and say why.
const ErrExclusiveUnavailable = "E_CONFINE_UNAVAILABLE"

// exclusiveRefusal builds that refusal, carrying the daemon's OWN code and
// message when it answered one. The distinction matters operationally: "another
// benchmark holds the slice" (retry later), "the daemon could not establish an
// empty slice" (something is wrong with cgroupfs), "your daemon is too old"
// (reinstall) and "the daemon is unreachable" all demand different actions, and
// a single generic message would hide which one happened.
func exclusiveRefusal(daemonCode, daemonMessage string) error {
	detail := strings.TrimSpace(daemonMessage)
	if detail == "" {
		detail = strings.TrimSpace(daemonCode)
	}
	if detail == "" {
		return fmt.Errorf("%s: --exclusive requires a daemon admission grant and the daemon did not provide one; refusing to launch non-exclusively", ErrExclusiveUnavailable)
	}
	return fmt.Errorf("%s: --exclusive requires a daemon admission grant; refusing to launch non-exclusively (%s)", ErrExclusiveUnavailable, detail)
}

func (r *Runner) admit(ctx context.Context, req Request) (admissionResult, error) {
	if req.NoAdmit {
		if req.Exclusive {
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"},
				exclusiveRefusal("", "admission is bypassed for this launch, so exclusivity cannot be established")
		}
		return admissionResult{state: "bypassed"}, nil
	}
	effectiveReserve := r.memoryReserve
	if req.MemoryReserveOverride != nil && *req.MemoryReserveOverride > 0 {
		effectiveReserve = *req.MemoryReserveOverride
	}
	if r.memorySlice == "" || r.memoryReserve == 0 {
		if req.Exclusive {
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"},
				exclusiveRefusal("", "admission is disabled for this launch, so exclusivity cannot be established")
		}
		return admissionResult{state: "disabled"}, nil
	}
	// AIRA-58: the shared wait ceiling, enforced before the daemon round trip. Since
	// S13 the admission wait no longer self-expires (design §4/§6: the client blocks
	// until granted, reconnecting across a daemon restart, and bounds the wait by ctx
	// cancellation), so r.admissionMaxWait no longer bounds anything on THIS path — it
	// is a flagged-vestigial §6 collision (see the S13 note; only confine-reserve still
	// applies its own MaxWait, as a ctx deadline). The ceiling survives as a synchronous
	// TYPO GUARD on the only setter that can produce an ARBITRARY value — the
	// `run.admission_max_wait` project-config key (admitConfine also sets it, but only to
	// the fixed DefaultConfineAdmissionWait): an absurd configured wait is refused with
	// the terminal code rather than accepted and silently ignored.
	if r.admissionMaxWait > AdmitWaitCeiling {
		return admissionResult{state: "wait_too_long", basis: "reject:wait-too-long"}, fmt.Errorf(
			"E_ADMIT_WAIT_TOO_LONG: requested admission wait %s exceeds the ceiling of %s",
			r.admissionMaxWait, AdmitWaitCeiling)
	}
	if result, granted, err := r.admitThroughDaemon(ctx, req, effectiveReserve); granted || err != nil {
		return result, err
	}
	// S13. admitThroughDaemon returns (granted=false, err=nil) ONLY when no daemon
	// socket is configured for this launch — a down or restarting daemon reconnects
	// indefinitely and never falls through here. The flock fallback is deleted, so
	// there is no self-gating path left: an EXCLUSIVE request refuses (exclusivity
	// cannot be established without the daemon ledger — AIRA-101), and everything else
	// launches `unevaluated` (ungoverned but warned; `--require-admission` refuses it,
	// AIRA-222).
	if req.Exclusive {
		return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"},
			exclusiveRefusal("", "no admission daemon is configured, so exclusivity cannot be established")
	}
	r.warnAdmission("unevaluated", "no-daemon")
	return admissionResult{state: "unevaluated", reason: "no-daemon"}, nil
}

// admitThroughDaemon runs the admission exchange and, on a TRANSPORT failure
// (daemon down or restarting mid-exchange) for a non-exclusive request, RECONNECTS
// at 2/sec with NO total deadline until the daemon answers or ctx is cancelled —
// the client fails CLOSED by waiting, never falling open to an ungoverned launch
// (design §4/§6; the AIRA-222 class this slice closes). A grant, a well-formed
// refusal, an exclusive incompletion, or ctx cancellation is terminal. The flock
// fallback is never entered from here.
func (r *Runner) admitThroughDaemon(ctx context.Context, req Request, effectiveReserve int64) (admissionResult, bool, error) {
	dial := r.admitDialFn
	if dial == nil {
		if strings.TrimSpace(r.admitSocketPath) == "" {
			return admissionResult{}, false, nil
		}
		dial = func(ctx context.Context, path string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		}
	}
	reconnectStart := r.clock.Now()
	lastNote := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			return admissionResult{}, false, err
		}
		result, granted, err := r.admitExchangeOnce(ctx, req, effectiveReserve, dial)
		if !errors.Is(err, errAdmitReconnect) {
			return result, granted, err
		}
		// Transport failure on a non-exclusive request. Retry 2/sec, indefinitely;
		// only ctx cancellation exits. A periodic "waiting for daemon" line (AIRA-71
		// shape) tells the operator why the launch is blocked.
		now := r.clock.Now()
		if lastNote.IsZero() || now.Sub(lastNote) >= 30*time.Second {
			r.warnDaemonWait(now.Sub(reconnectStart))
			lastNote = now
		}
		select {
		case <-ctx.Done():
			return admissionResult{}, false, ctx.Err()
		case <-r.clock.After(leaseKeeperReconnectGap):
		}
	}
}

// admitExchangeOnce performs ONE dial + admission frame exchange. It returns a
// grant (the connection wrapped in a lease keeper as admissionResult.release), a
// terminal refusal (err set), a ctx-cancellation, or the errAdmitReconnect sentinel
// for a NON-exclusive transport failure (which admitThroughDaemon turns into a
// reconnect). An EXCLUSIVE request never reconnects: any incomplete exchange refuses
// (exclusivity cannot be silently re-tried into a contended launch —
// admit_exclusive_unwedge stays green).
func (r *Runner) admitExchangeOnce(ctx context.Context, req Request, effectiveReserve int64, dial func(context.Context, string) (net.Conn, error)) (admissionResult, bool, error) {
	admissionStarted := time.Now()
	// Dial with a bounded 500 ms timeout (design §4). There is NO transport deadline
	// on the exchange itself: §4/§6 specify no client deadline, so a long legitimate
	// wait must not tear its own connection down — that would drop the request and
	// reconnect it to a fresh FIFO position.
	dctx, cancelDial := context.WithTimeout(ctx, leaseKeeperDialTimeout)
	conn, err := dial(dctx, r.admitSocketPath)
	cancelDial()
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return admissionResult{}, false, cerr
		}
		if req.Exclusive {
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"}, false,
				exclusiveRefusal("", "the daemon was unreachable for an exclusive admission request")
		}
		return admissionResult{}, false, errAdmitReconnect
	}
	monitorStop := make(chan struct{})
	monitorDone := make(chan struct{})
	monitorErr := make(chan error, 1)
	var monitorStopOnce sync.Once
	stopMonitor := func() { monitorStopOnce.Do(func() { close(monitorStop) }) }
	if req.Detach {
		if err := r.checkDetachAdmission(req); err != nil {
			_ = conn.Close()
			return admissionResult{}, false, err
		}
		interval := r.pollInterval
		if interval <= 0 {
			interval = 2 * time.Second
		}
		go func() {
			defer close(monitorDone)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-monitorStop:
					return
				case <-ticker.C:
					if err := r.checkDetachAdmission(req); err != nil {
						monitorErr <- err
						_ = conn.Close()
						return
					}
				}
			}
		}()
		defer stopMonitor()
	} else {
		close(monitorDone)
	}
	// fail closes the socket and classifies the failure (S13). A NON-exclusive
	// TRANSPORT failure (write/read of the frame failed — the daemon EOF'd or was
	// mid-restart) becomes errAdmitReconnect, and admitThroughDaemon reconnects; an
	// EXCLUSIVE one refuses (exclusivity is never silently re-tried into a contended
	// launch); a detach kill-intent or ctx cancellation is terminal. The flock
	// fallback is never reached from here — the client fails CLOSED by waiting.
	fail := func() (admissionResult, bool, error) {
		_ = conn.Close()
		select {
		case err := <-monitorErr:
			return admissionResult{}, false, err
		default:
		}
		if err := ctx.Err(); err != nil {
			return admissionResult{}, false, err
		}
		if req.Detach {
			if err := r.checkDetachAdmission(req); err != nil {
				return admissionResult{}, false, err
			}
		}
		if req.Exclusive {
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"}, false,
				exclusiveRefusal("", "the exclusive admission exchange with the daemon did not complete")
		}
		return admissionResult{}, false, errAdmitReconnect
	}

	// Only caller cancellation closes the conn asynchronously: a full frame that
	// completes must win and keep this lease open through Start. No transport
	// deadline — §4/§6 specify no client deadline (see admitExchangeOnce).
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	frame := runnerAdmitRequestFrame{Proto: DaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "admit"
	// S5. cpu is the second ledger resource on the CONFINE admission path. An ordinary
	// `aira confine` AND an `aira confine-reserve` sub-reservation (confine_reserve_linux.go,
	// no DelegateRAM) declare the one-core default (design §9); the daemon charges each
	// against the per-slice 2×NumCPU ceiling at admission. A --delegate-ram job declares
	// 0 cores (spec §8): it is framework overhead and charging it a core would double-count.
	//
	// aitest pytest workers are bounded too, since S15: they reach the daemon via
	// worker-admit, which now charges each worker's one core against the SAME per-slice
	// 2×NumCPU ledger (the worker lease is an ordinary signed-ledger lease). Accounting
	// only — no cpu.max is written. The S5 `cpu` arg's ProtocolVersion bump landed in S7:
	// both DaemonProtocolVersion and daemon.ProtocolVersion are 10, in lockstep.
	cpuCores := DefaultConfineCPUCores
	if req.DelegateRAM {
		cpuCores = 0
	}
	// S13: NO max_wait_ms. The admission wait no longer self-expires (design §4/§6):
	// the client blocks until granted and reconnects across a daemon restart, bounding
	// the wait by ctx cancellation, never by a daemon-side timeout. An absent
	// max_wait_ms is a blocking request to the daemon.
	frame.Request.Args = map[string]any{
		"slice":     r.memorySlice,
		"reserve":   effectiveReserve,
		"cpu":       cpuCores,
		"signature": req.ResourceSignature,
		"pinned":    !req.DaemonEstimateMemory || req.MemoryReservePinned,
	}
	if req.DelegateRAM {
		frame.Request.Args["delegate_ram"] = true
	}
	if req.ConfineScopeID != "" {
		frame.Request.Args["scope_id"] = req.ConfineScopeID
		frame.Request.Args["name"] = req.ConfineName
		frame.Request.Args["owner"] = req.ConfineOwner
	}
	// AIRA-101. All three are optional and absent-means-off, so an older daemon
	// is not confused by their presence — it REJECTS them with E_DAEMON_PROTOCOL,
	// which for an exclusive request is exactly right: admitExclusiveOrRefuse
	// turns that into a loud refusal rather than a silently non-exclusive launch.
	if req.Exclusive {
		frame.Request.Args["exclusive"] = true
		// AIRA-185. Guarded by req.Exclusive rather than sent whenever it is set:
		// the daemon refuses `reason` on a non-exclusive request (it has no
		// exclusive state to attribute it to and will not accept-and-discard it), so
		// sending it unconditionally would turn a stray label into a REFUSED launch
		// of an otherwise fine ordinary job. A reason on a non-exclusive
		// runner.Request is therefore dropped HERE, at the one place that could
		// otherwise weaponise it — and no CLI face can produce that combination,
		// because `--reason` exists only on `aira drain wait`, which always asks for
		// exclusivity.
		if reason := strings.TrimSpace(req.ExclusiveReason); reason != "" {
			frame.Request.Args["reason"] = reason
		}
	}
	if req.ExclusiveHolder != "" {
		frame.Request.Args["exclusive_holder"] = req.ExclusiveHolder
	}
	if req.ParentScopeID != "" {
		frame.Request.Args["parent_scope_id"] = req.ParentScopeID
	}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		return fail()
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		return fail()
	}
	if !response.OK || response.Code != "OK" {
		// AIRA-58: a wait-ceiling refusal is TERMINAL and must never reach fail(),
		// which routes to the flock fallback and would launch the job outside the
		// daemon ledger — turning a refusal into an unaccounted admission, strictly
		// worse than the silent clamp it replaced. Deliberately handled BEFORE the
		// structured-payload branch and WITHOUT depending on the payload parsing,
		// so a malformed or absent rejection body still refuses rather than
		// degrading. Any future admit-path refusal code needs the same treatment.
		// AIRA-101. The two exclusive refusals join this TERMINAL pre-payload
		// block for the same reason E_ADMIT_WAIT_TOO_LONG is here: anything the
		// runner does not explicitly recognise falls through to fail() and the
		// flock fallback. For an exclusive request that would be doubly wrong —
		// the job would launch both unaccounted AND non-exclusive while its
		// operator believed otherwise. Handled before the structured-payload
		// branch so a malformed body still refuses rather than degrading.
		//
		// E_DAEMON_PROTOCOL is included because that is precisely what an OLDER
		// daemon answers to the unknown `exclusive` field: version skew must be
		// loud, not a silently contended benchmark.
		if req.Exclusive {
			switch response.Code {
			case "E_ADMIT_EXCLUSIVE_ACTIVE", "U_ADMIT_EXCLUSIVE_UNESTABLISHED", "E_DAEMON_PROTOCOL", "E_DAEMON_BUSY":
				_ = conn.Close()
				return admissionResult{
					state:    "exclusive_unavailable",
					waitedMS: time.Since(admissionStarted).Milliseconds(),
					reserve:  effectiveReserve,
					basis:    "reject:exclusive-unavailable",
				}, true, exclusiveRefusal(response.Code, response.Error)
			}
		}
		if response.Code == "E_ADMIT_WAIT_TOO_LONG" {
			_ = conn.Close()
			message := strings.TrimSpace(response.Error)
			if message == "" {
				message = response.Code + ": requested admission wait exceeds the daemon ceiling"
			}
			return admissionResult{
				state:    "wait_too_long",
				waitedMS: time.Since(admissionStarted).Milliseconds(),
				reserve:  effectiveReserve,
				basis:    "reject:wait-too-long",
			}, true, errors.New(message)
		}
		if response.Code == "E_ADMIT_TOO_LARGE" || response.Code == "E_ADMIT_SATURATED" {
			var rejection runnerAdmitRejection
			if err := json.Unmarshal(response.Data, &rejection); err == nil && validRunnerAdmitRejection(response.Code, rejection) {
				_ = conn.Close()
				resolved := rejection.Required
				if resolved <= 0 {
					resolved = effectiveReserve
				}
				basis := "reject:saturated"
				message := response.Error
				if response.Code == "E_ADMIT_TOO_LARGE" {
					basis = "reject:too-large"
				} else {
					ceiling := "unknown"
					if rejection.Ceiling > 0 {
						ceiling = FormatConfineBytes(rejection.Ceiling)
					}
					// Honest wording: E_ADMIT_SATURATED means the wait window expired
					// without a grant — the slice was contended for the duration — not
					// that it is persistently "genuinely saturated" (a state the daemon
					// never establishes on this path).
					//
					// AIRA-101. When the wait expired under slice EXCLUSIVITY, say so.
					// The default clause below is a MEMORY diagnosis, and offering it for
					// a benchmark holding the slice sends the operator to look at RAM for
					// something that has nothing to do with RAM.
					switch rejection.Exclusive {
					case "held":
						message = fmt.Sprintf("E_ADMIT_SATURATED: confine: admission rejected after %s — the slice is held exclusively by another job for benchmarking; retry when it finishes (reserve %s/%s)", time.Since(admissionStarted).Round(time.Second), FormatConfineBytes(resolved), ceiling)
					case "draining":
						// Covers BOTH a bystander waiting behind somebody's drain and the
						// exclusive requester's own drain failing to complete in its budget.
						// Neither is a memory problem.
						message = fmt.Sprintf("E_ADMIT_SATURATED: confine: admission rejected after %s — the slice was draining for an exclusive job and the drain did not complete within the wait (reserve %s/%s)", time.Since(admissionStarted).Round(time.Second), FormatConfineBytes(resolved), ceiling)
					default:
						// AIRA-149. The daemon's LATCHED contention reading replaces a
						// manufactured cause. "slice contended, no memory admission within
						// the wait" used to be asserted for every non-exclusive rejection,
						// including one where the daemon never observed anything else
						// holding or queued ahead -- the measured case, where a resolved
						// reserve equal to the ceiling simply could not fit a slice carrying
						// one residual page.
						//
						// An EMPTY value is a fourth state, "not reported by this daemon
						// build", and keeps the existing sentence: it must never be read as
						// an established solitude.
						elapsed := time.Since(admissionStarted).Round(time.Second)
						switch rejection.Contention {
						case "none-observed":
							// The ceiling is the REQUEST-ENTRY figure and the grantable is the
							// gate's LAST PASS; they are different instants, so each is labelled
							// by its own provenance and the sentence never invites the reader to
							// subtract one from the other. "queued AHEAD of this request" is the
							// fact that was established -- a waiter queued behind was never in
							// this request's way -- and it is deliberately narrower than "the
							// slice was empty", which would be a new fabrication: the slice's own
							// residual charge is exactly why the request failed.
							message = fmt.Sprintf("E_ADMIT_SATURATED: confine: admission rejected after %s — nothing else was running in this slice or queued ahead of this request at any evaluation; the resolved reserve %s did not fit the admission ceiling %s%s. Pin --memory-reserve or --memory-max to size this job yourself.",
								elapsed, FormatConfineBytes(resolved), ceiling, formatGrantableClause(rejection.Grantable))
						case "unevaluated":
							// Covers all three causes without naming one: the deadline fired
							// before any pass, the slice memory read failed for the whole wait,
							// or a failing confine scan left emptiness unestablished. "Could not
							// establish" is true of all three.
							message = fmt.Sprintf("E_ADMIT_SATURATED: confine: admission rejected after %s — the admission gate could not establish this request's contention before the wait expired (reserve %s/%s)",
								elapsed, FormatConfineBytes(resolved), ceiling)
						default:
							message = fmt.Sprintf("E_ADMIT_SATURATED: confine: admission rejected after %s — slice contended, no memory admission within the wait (reserve %s/%s)",
								elapsed, FormatConfineBytes(resolved), ceiling)
						}
					}
				}
				if message == "" {
					message = response.Code + ": " + rejection.Basis
				}
				return admissionResult{state: strings.TrimPrefix(strings.ToLower(response.Code), "e_admit_"), waitedMS: time.Since(admissionStarted).Milliseconds(), reserve: resolved, ceiling: rejection.Ceiling, basis: basis}, true, errors.New(message)
			}
		}
		// S13. A WELL-FORMED refusal frame with a code the client does not recognise (a
		// fail-closed CodeUnavailable, a CodeBusy, or a version-skew E_DAEMON_PROTOCOL)
		// is a genuine refusal, NOT a transport failure. It is TERMINAL: refuse to
		// launch and surface the daemon's reason. It never reconnects (that would loop
		// on the same refusal) and never falls open to an ungoverned launch — the
		// deleted flock fallback's failure mode this slice exists to close.
		_ = conn.Close()
		message := strings.TrimSpace(response.Error)
		if message == "" {
			message = strings.TrimSpace(response.Code)
		}
		if message == "" {
			message = "the daemon refused admission"
		}
		return admissionResult{state: "refused", waitedMS: time.Since(admissionStarted).Milliseconds(), reserve: effectiveReserve, basis: "reject:daemon-refused"}, false,
			fmt.Errorf("%s; refusing to launch ungoverned", message)
	}
	var grant runnerAdmitGrant
	if err := json.Unmarshal(response.Data, &grant); err != nil || !validRunnerAdmitGrant(grant) {
		if req.Exclusive {
			_ = conn.Close()
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"}, false,
				exclusiveRefusal("", "the daemon's admission grant could not be read")
		}
		// S13. A malformed grant is a daemon protocol fault, not a transport failure:
		// reconnecting would re-fetch the same bad frame. TERMINAL refuse, never fall open.
		_ = conn.Close()
		return admissionResult{state: "refused", reserve: effectiveReserve, basis: "reject:daemon-refused"}, false,
			errors.New("E_DAEMON_PROTOCOL: the daemon's admission grant could not be read; refusing to launch ungoverned")
	}
	// AIRA-101. `unevaluated` is a real grant state — the daemon answered, but
	// could not establish the slice's usage — and an ordinary job proceeds on it
	// uncapped-but-launched. An EXCLUSIVE job must not: "the daemon could not
	// evaluate the slice" is precisely the case where a claim of exclusivity would
	// be fabricated. Only a genuine immediate/waited grant is exclusivity.
	if req.Exclusive && grant.State != "immediate" && grant.State != "waited" {
		_ = conn.Close()
		return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"}, false,
			exclusiveRefusal("", "the daemon answered "+grant.State+" rather than granting exclusive admission")
	}
	// A full, validated frame claims the connection as the lease. Before returning
	// it, STOP the async closers so neither the ctx callback nor the detach monitor
	// can close the lease after we hand it back (Sol build r1 #3, r2 #3).
	// ctx-callback arbitration: stopClose() returns false iff the ctx callback has
	// already started (ctx is Done) — the closer won, so abort with the ctx error
	// rather than return a lease it is closing.
	if !stopClose() {
		_ = conn.Close()
		return admissionResult{}, false, ctx.Err()
	}
	// detach-monitor arbitration: stop + JOIN the monitor, then honour a kill-intent
	// that raced the grant (closer wins -> abort).
	stopMonitor()
	<-monitorDone
	select {
	case err := <-monitorErr:
		_ = conn.Close()
		return admissionResult{}, false, err
	default:
	}
	// S13. The granted connection is the LEASE. Wrap it in a lease keeper: for a
	// scope-bearing non-exclusive grant the keeper re-declares the lease across a
	// daemon restart (design §4); a scope-less confine-reserve lease is held only;
	// an exclusive lease is watched by the confine caller (watchExclusive). The
	// keeper's Close() is admissionResult.release — teardown closes it, which the
	// daemon reads as the lease-releasing EOF. No transport deadline was ever set on
	// this conn, so a held lease that outlives its admission wait is never torn down.
	keeper := newLeaseKeeper(conn, req, grant, dial, r.admitSocketPath)
	return admissionResult{state: grant.State, reason: grant.Reason, waitedMS: grant.WaitedMS, release: keeper, reserve: grant.Reserve, basis: grant.Basis, scopeCeiling: grant.ScopeCeiling}, true, nil
}

// warnDaemonWait emits the periodic "waiting for the daemon" line while the client
// reconnects across a daemon restart (design §4, AIRA-71 UX shape). It never fails
// open — the launch is simply blocked until the daemon returns or ctx is cancelled.
func (r *Runner) warnDaemonWait(waited time.Duration) {
	if r.diagnostics == nil {
		return
	}
	_, _ = fmt.Fprintf(r.diagnostics, "aira: waiting for the memory-admission daemon to become reachable (waited %s); the launch is held, not run ungoverned\n", waited.Round(time.Second))
}

const mathMaxInt64 = int64(^uint64(0) >> 1)

func (r *Runner) checkDetachAdmission(req Request) error {
	if !req.Detach {
		return nil
	}
	current, err := r.ledger.current(req.detachRunID)
	if err != nil {
		return launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	if current.Detached && current.KillIntent.Present {
		return errDetachKillIntent
	}
	return nil
}

func validRunnerAdmitGrant(grant runnerAdmitGrant) bool {
	if grant.WaitedMS < 0 || grant.Reserve <= 0 || strings.TrimSpace(grant.Basis) == "" {
		return false
	}
	switch grant.State {
	case "immediate", "waited", "unevaluated":
		return true
	default:
		return false
	}
}

// formatGrantableClause renders the daemon's last-measured grantable figure, or
// nothing at all when the daemon did not report one (AIRA-149).
//
// A MEASURED ZERO must not be rendered through FormatConfineBytes, which
// returns the string "unknown" for 0 -- this codebase's word for NOT
// ESTABLISHED. Passing a measured zero through it would turn the honest reading
// "not one byte was grantable" into "the daemon does not know", which is exactly
// the conflation this change exists to remove.
func formatGrantableClause(grantable *int64) string {
	if grantable == nil {
		return ""
	}
	// S4: the ledger is signed, so `available` can be NEGATIVE (the slice is
	// over-subscribed during the restart re-declare window). Render that deficit
	// honestly rather than flattening it to "0B", which would read as "nothing
	// grantable right now" and hide that a release must first recover the ledger.
	if *grantable < 0 {
		return " (slice over-subscribed by " + FormatConfineBytes(-*grantable) + " at the last evaluation)"
	}
	text := "0B"
	if *grantable > 0 {
		text = FormatConfineBytes(*grantable)
	}
	return " (largest grantable reserve " + text + " at the last evaluation)"
}

func validRunnerAdmitRejection(code string, rejection runnerAdmitRejection) bool {
	switch code {
	case "E_ADMIT_TOO_LARGE":
		return rejection.Required > 0 && rejection.Ceiling >= 0 && strings.TrimSpace(rejection.Basis) != ""
	case "E_ADMIT_SATURATED":
		return rejection.Basis == "reject:saturated"
	default:
		return false
	}
}

func writeRunnerAdmitFrame(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > runnerDaemonMaxFrameBytes {
		return errWorkerAdmitFrameSize
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeRunnerAdmitBytes(w, header[:]); err != nil {
		return err
	}
	return writeRunnerAdmitBytes(w, payload)
}

func writeRunnerAdmitBytes(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func reportConfinePeak(ctx context.Context, request ConfineRequest, report ConfinePeakReport) error {
	if strings.TrimSpace(request.AdmitSocketPath) == "" || report.Signature == "" {
		return errors.New("daemon report unavailable")
	}
	return ReportPeakSample(ctx, request.AdmitSocketPath, report)
}

// ReportPeakSample sends one usage sample to the daemon over the project-less
// admit socket. Exported because the aitest supervisor's pool sample travels the
// same verb through the `aira worker-peak` relay, and a second transport for the
// same frame is exactly the kind of duplicate that drifts.
func ReportPeakSample(ctx context.Context, socketPath string, report ConfinePeakReport) error {
	if strings.TrimSpace(socketPath) == "" || report.Signature == "" {
		return errors.New("daemon report unavailable")
	}
	request := ConfineRequest{AdmitSocketPath: socketPath}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", request.AdmitSocketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	frame := runnerAdmitRequestFrame{Proto: DaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "confine-report"
	frame.Request.Args = map[string]any{"signature": report.Signature, "oom": report.OOM}
	if report.Kind != "" {
		frame.Request.Args["kind"] = report.Kind
	}
	if report.Peak != nil && *report.Peak > 0 {
		frame.Request.Args["peak_rss"] = *report.Peak
	}
	// The pair travels together or not at all: the daemon refuses half of it,
	// because a budget with no provenance is a quantity the classifier cannot
	// compare with anything.
	if report.Budget != nil && *report.Budget > 0 && report.BudgetBasis != "" {
		frame.Request.Args["budget"] = *report.Budget
		frame.Request.Args["budget_basis"] = report.BudgetBasis
	}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		return err
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		return err
	}
	if !response.OK || response.Code != "OK" {
		if response.Error != "" {
			return errors.New(response.Error)
		}
		return errors.New("daemon rejected confine report")
	}
	return nil
}

func readRunnerAdmitFrame(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > runnerDaemonMaxFrameBytes {
		return errWorkerAdmitFrameSize
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, value)
}

func jitteredPoll(interval time.Duration, iteration uint64) time.Duration {
	// Deterministic +/-10% jitter avoids a shared random source and keeps fake
	// clock tests exact at the max-wait clamp.
	switch iteration % 3 {
	case 1:
		return interval + interval/10
	case 2:
		return interval - interval/10
	default:
		return interval
	}
}

func (r *Runner) warnAdmission(state, reason string) {
	if r.diagnostics == nil {
		return
	}
	if reason == "" {
		_, _ = fmt.Fprintf(r.diagnostics, "aira: warning: memory admission %s; launching ungoverned\n", state)
		return
	}
	_, _ = fmt.Fprintf(r.diagnostics, "aira: warning: memory admission %s (%s); launching ungoverned\n", state, reason)
}

func resolveSlicePath(slice string) (string, bool, string) {
	mount, err := unifiedMount()
	if err != nil {
		return "", false, "slice-not-found"
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		return "", false, "slice-not-found"
	}
	return resolveSlicePathAt(slice, mount, current)
}

// resolveSlicePathExact is the error-preserving counterpart used by confine's
// aira→whale default policy. The older resolver intentionally collapses every
// failure for admission callers; default confinement must distinguish definite
// ENOENT from permission and evaluation failures.
func resolveSlicePathExact(slice string) (string, error) {
	mount, err := unifiedMount()
	if err != nil {
		return "", err
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		return "", err
	}
	return resolveSlicePathAtExact(slice, mount, current)
}

func resolveSlicePathAtExact(slice, mount, current string) (string, error) {
	slice = strings.TrimSpace(slice)
	if slice == "" || hasParentComponent(slice) {
		return "", fs.ErrNotExist
	}
	mountAbs, err := filepath.Abs(mount)
	if err != nil {
		return "", err
	}
	mountCanonical, err := filepath.EvalSymlinks(mountAbs)
	if err != nil {
		return "", err
	}
	var candidates []string
	if filepath.IsAbs(slice) {
		candidates = []string{slice}
	} else if !strings.ContainsRune(slice, filepath.Separator) && strings.HasSuffix(slice, ".slice") {
		for cursor := filepath.Clean(current); pathWithin(mountCanonical, cursor); cursor = filepath.Dir(cursor) {
			if filepath.Base(cursor) == slice {
				candidates = append(candidates, cursor)
			}
			candidates = append(candidates, filepath.Join(cursor, slice))
			if filepath.Clean(cursor) == filepath.Clean(mountCanonical) {
				break
			}
		}
	} else {
		candidates = []string{filepath.Join(mountCanonical, slice)}
	}
	var firstFailure error
	for _, candidate := range candidates {
		candidateAbs, absErr := filepath.Abs(candidate)
		if absErr != nil {
			if firstFailure == nil {
				firstFailure = absErr
			}
			continue
		}
		if !pathWithin(mountCanonical, candidateAbs) {
			continue
		}
		canonical, evalErr := filepath.EvalSymlinks(candidateAbs)
		if evalErr != nil {
			if !errors.Is(evalErr, fs.ErrNotExist) && firstFailure == nil {
				firstFailure = evalErr
			}
			continue
		}
		if !pathWithin(mountCanonical, canonical) {
			continue
		}
		info, statErr := os.Stat(canonical)
		if statErr == nil && info.IsDir() {
			return canonical, nil
		}
		if statErr == nil {
			statErr = fmt.Errorf("%s is not a directory", canonical)
		}
		if !errors.Is(statErr, fs.ErrNotExist) && firstFailure == nil {
			firstFailure = statErr
		}
	}
	if firstFailure != nil {
		return "", firstFailure
	}
	return "", fs.ErrNotExist
}

func resolveSlicePathAt(slice, mount, current string) (string, bool, string) {
	slice = strings.TrimSpace(slice)
	if slice == "" || hasParentComponent(slice) {
		return "", false, "slice-not-found"
	}
	mountAbs, err := filepath.Abs(mount)
	if err != nil {
		return "", false, "slice-not-found"
	}
	mountCanonical, err := filepath.EvalSymlinks(mountAbs)
	if err != nil {
		return "", false, "slice-not-found"
	}
	var candidates []string
	if filepath.IsAbs(slice) {
		candidates = []string{slice}
	} else if !strings.ContainsRune(slice, filepath.Separator) && strings.HasSuffix(slice, ".slice") {
		for cursor := filepath.Clean(current); pathWithin(mountCanonical, cursor); cursor = filepath.Dir(cursor) {
			if filepath.Base(cursor) == slice {
				candidates = append(candidates, cursor)
			}
			candidates = append(candidates, filepath.Join(cursor, slice))
			if filepath.Clean(cursor) == filepath.Clean(mountCanonical) {
				break
			}
		}
	} else {
		candidates = []string{filepath.Join(mountCanonical, slice)}
	}
	for _, candidate := range candidates {
		candidateAbs, absErr := filepath.Abs(candidate)
		if absErr != nil || !pathWithin(mountCanonical, candidateAbs) {
			continue
		}
		canonical, evalErr := filepath.EvalSymlinks(candidateAbs)
		if evalErr != nil || !pathWithin(mountCanonical, canonical) {
			continue
		}
		st, statErr := os.Stat(canonical)
		if statErr == nil && st.IsDir() {
			return canonical, true, ""
		}
	}
	return "", false, "slice-not-found"
}

func hasParentComponent(path string) bool {
	for _, component := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if component == ".." {
			return true
		}
	}
	return false
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func readSliceMemory(path string) (cur, max int64, ok bool, reason string) {
	currentData, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, 0, false, "read-error"
	}
	maxData, err := os.ReadFile(filepath.Join(path, "memory.max"))
	if err != nil {
		return 0, 0, false, "read-error"
	}
	current, valid := parseAdmissionMemory(currentData)
	if !valid {
		return 0, 0, false, "parse-error"
	}
	maxText := strings.TrimSpace(string(maxData))
	if maxText == "max" {
		return 0, 0, false, "unbounded"
	}
	limit, valid := parseAdmissionMemory(maxData)
	if !valid {
		return 0, 0, false, "parse-error"
	}
	return current, limit, true, ""
}

func parseAdmissionMemory(data []byte) (int64, bool) {
	text := strings.TrimSpace(string(data))
	if text == "" || len(strings.Fields(text)) != 1 {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil && value >= 0
}
