package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-324. The flat charge discount for scope-backed --memory-reserve pins:
// booking = clamp(ceil(factor x pin), 1 MiB, pin), grant echo and cap = pin,
// physical-fit rule, modes off/observe/enforce. Spec section 5.2 and tests 1-6 of
// section 8. Every test names the mutation it reds against; the mutations are
// applied and run by docs/dev/aira-324-mutation-check.py.

// pinServer is a slice of `maximum` bytes, zero headroom, a caller-controlled
// physical reading (memory.current, no reclaimable) and the given mode at the
// default 0.8 factor.
func pinServer(t *testing.T, mode pinChargeMode, maximum int64, current *int64, now *time.Time) *Server {
	t.Helper()
	var max atomic.Int64
	max.Store(maximum)
	server := admitTestServer(&max)
	server.admitNow = func() time.Time { return *now }
	server.admitFreezeMaxHold = time.Minute
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return *current, max.Load(), 0, true, ""
	}
	server.pinChargeMode = mode
	server.pinChargePercent = pinChargeFactorDefault
	return server
}

// pinWaiter is a queued, scope-backed, discountable waiter whose booking was set
// the way enqueueAdmitInternal sets it on this server.
func pinWaiter(server *Server, seq, pin int64, now time.Time) *admitWaiter {
	waiter := queuedWaiter(seq, pin, now)
	waiter.scopeID = fmt.Sprintf("CONFINE-pin%d-%d-%s", seq, 7000+seq, "a")
	waiter.discountable = true
	waiter.charge = bookingField(server.pinChargeFor(true, pin), pin)
	return waiter
}

// verifies: AIRA-324 rule — booking = clamp(ceil(factor x pin), 1 MiB, pin), exact
// integer arithmetic. Pins are chosen not divisible by 5 or 10 so a floor, a
// round-to-nearest or a float product is off by at least one byte. Mutations:
// floor instead of ceil; drop the 1 MiB floor; drop the pin clamp; apply the
// factor at 100 (factor 1.0 must book the pin).
func TestDiscountedPinChargeIsCeilClampedAndExact(t *testing.T) {
	const gi = int64(1) << 30
	for _, tc := range []struct {
		name    string
		pin     int64
		percent int64
		want    int64
	}{
		{"0.8 of an odd pin rounds UP", 5*gi + 3, 80, (4*(5*gi+3) + 4) / 5},
		{"0.9 of an odd pin rounds UP", 7*gi + 1, 90, (9*(7*gi+1) + 9) / 10},
		{"0.85", 3*gi + 7, 85, (85*(3*gi+7) + 99) / 100},
		{"factor 1.0 books the pin", 5*gi + 3, 100, 5*gi + 3},
		{"the 1 MiB floor binds", (1 << 20) + 1, 80, 1 << 20},
		{"a pin at the floor books itself", 1 << 20, 80, 1 << 20},
		{"a pin below the floor books itself", 512 << 10, 50, 512 << 10},
		{"a pin above admitMaxReserve books itself", admitMaxReserve + 1, 80, admitMaxReserve + 1},
		{"0.5", 2*gi + 1, 50, gi + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := discountedPinCharge(tc.pin, tc.percent); got != tc.want {
				t.Fatalf("discountedPinCharge(%d, %d) = %d, want %d", tc.pin, tc.percent, got, tc.want)
			}
		})
	}
	// The exact-product property, over a sweep: never below factor x pin, never a
	// whole byte above it.
	for pin := int64(3 << 20); pin < 3<<20+2000; pin += 7 {
		got := discountedPinCharge(pin, 80)
		if got*100 < pin*80 || (got-1)*100 >= pin*80 {
			t.Fatalf("pin %d: charge %d is not ceil(0.8 x pin)", pin, got)
		}
	}
}

// verifies: AIRA-324 factor knob — a decimal with at most two places in
// [0.5, 1], refused (never rounded) otherwise; the factor is read only when the
// mode is not off, so a typo cannot stop a daemon that did not ask for the
// feature. Mutations: accept three places (rounding); drop either bound; parse
// the factor when the mode is off.
func TestPinChargeFactorAndModeParsing(t *testing.T) {
	for raw, want := range map[string]int64{"0.8": 80, "0.80": 80, "0.85": 85, "0.9": 90, "1": 100, "1.0": 100, "1.00": 100, "0.5": 50, " 0.75 ": 75} {
		if got, ok := runner.ParsePinChargeFactor(raw); !ok || got != want {
			t.Fatalf("ParsePinChargeFactor(%q) = %d, %v; want %d, true", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "0.49", "1.01", "0.855", ".8", "0.", "08", "00.8", "1e0", "-0.8", "+0.8", "abc", "0,8", "2", "0.8x"} {
		if got, ok := runner.ParsePinChargeFactor(raw); ok {
			t.Fatalf("ParsePinChargeFactor(%q) = %d, accepted; want refused", raw, got)
		}
	}

	t.Setenv("AIRA_DAEMON_PIN_CHARGE_MODE", "")
	t.Setenv("AIRA_DAEMON_PIN_CHARGE_FACTOR", "not-a-number")
	if mode, percent, err := pinChargeConfigFromEnv(); err != nil || mode != pinChargeOff || percent != pinChargeFactorDefault {
		t.Fatalf("unset mode = %q, %d, %v; want off, the default factor, no error (the factor is not read when off)", mode, percent, err)
	}
	t.Setenv("AIRA_DAEMON_PIN_CHARGE_MODE", "observe")
	if _, _, err := pinChargeConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
		t.Fatalf("observe with a bad factor: err=%v, want E_CONFIG_INVALID", err)
	}
	t.Setenv("AIRA_DAEMON_PIN_CHARGE_FACTOR", "0.9")
	if mode, percent, err := pinChargeConfigFromEnv(); err != nil || mode != pinChargeObserve || percent != 90 {
		t.Fatalf("observe 0.9 = %q, %d, %v", mode, percent, err)
	}
	t.Setenv("AIRA_DAEMON_PIN_CHARGE_FACTOR", "")
	t.Setenv("AIRA_DAEMON_PIN_CHARGE_MODE", "enforce")
	// The literal 80, not the constant: a changed default must fail here.
	if mode, percent, err := pinChargeConfigFromEnv(); err != nil || mode != pinChargeEnforce || percent != 80 {
		t.Fatalf("enforce default = %q, %d, %v", mode, percent, err)
	}
	t.Setenv("AIRA_DAEMON_PIN_CHARGE_MODE", "on")
	if _, _, err := pinChargeConfigFromEnv(); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

// verifies: AIRA-324 section 8 test 2 and the mode switch — only enforce, outside
// ci-shim mode, for a DISCOUNTABLE request, books below the pin. Mutations:
// observe applies the discount; the shim guard dropped; discountability inferred
// from anything but the flag (the daemon has no other input here).
func TestPinChargeForOnlyEnforceDiscountsADiscountableRequest(t *testing.T) {
	const pin = int64(10<<30) + 5
	discounted := discountedPinCharge(pin, pinChargeFactorDefault)
	for _, tc := range []struct {
		mode         pinChargeMode
		shim         bool
		discountable bool
		want         int64
	}{
		{pinChargeEnforce, false, true, discounted},
		{pinChargeEnforce, false, false, pin},
		{pinChargeEnforce, true, true, pin},
		{pinChargeObserve, false, true, pin},
		{pinChargeOff, false, true, pin},
		{"", false, true, pin},
	} {
		t.Run(fmt.Sprintf("%s/shim=%t/discountable=%t", tc.mode, tc.shim, tc.discountable), func(t *testing.T) {
			server := NewServer(Paths{})
			server.pinChargeMode, server.pinChargePercent = tc.mode, pinChargeFactorDefault
			if tc.shim {
				server.confineMode = runner.ConfineModeShim
			}
			if got := server.pinChargeFor(tc.discountable, pin); got != tc.want {
				t.Fatalf("pinChargeFor = %d, want %d", got, tc.want)
			}
		})
	}
}

// verifies: AIRA-324 section 8 test 2 — the admit field is validated and refused
// on every request the discount must never reach; a pinned request WITHOUT the
// field is never discounted (the daemon does not infer it from `pinned`, which
// `aira run` and confine-reserve also send). Mutations: drop any one refusal;
// accept a non-boolean; infer discountable from pinned.
func TestValidateAdmitArgsDiscountable(t *testing.T) {
	scoped := func(extra map[string]any) map[string]any {
		args := map[string]any{
			"slice": "aira.slice", "reserve": int64(4 << 30), "pinned": true,
			"scope_id": "CONFINE-job-123-a", "name": "job", "owner": runner.ConfineUnknownOwner,
		}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	request, err := validateAdmitArgs(scoped(map[string]any{"discountable": true}), admitWaitCeilingMs)
	if err != nil || !request.discountable {
		t.Fatalf("a scoped pinned discountable request: discountable=%v err=%v", request.discountable, err)
	}
	if request, err := validateAdmitArgs(scoped(map[string]any{"discountable": false}), admitWaitCeilingMs); err != nil || request.discountable {
		t.Fatalf("discountable=false: discountable=%v err=%v", request.discountable, err)
	}
	if request, err := validateAdmitArgs(scoped(nil), admitWaitCeilingMs); err != nil || request.discountable {
		t.Fatalf("pinned without the field must not be discountable: %v %v", request.discountable, err)
	}
	unpinned := scoped(map[string]any{"discountable": true})
	delete(unpinned, "pinned")
	scopeless := map[string]any{"slice": "aira.slice", "reserve": int64(4 << 30), "pinned": true, "discountable": true}
	for name, args := range map[string]map[string]any{
		"non-boolean":     scoped(map[string]any{"discountable": "true"}),
		"unpinned":        unpinned,
		"pinned false":    scoped(map[string]any{"discountable": true, "pinned": false}),
		"exclusive":       scoped(map[string]any{"discountable": true, "exclusive": true}),
		"scope-less":      scopeless,
		"parent scope id": {"slice": "aira.slice", "reserve": int64(1 << 20), "pinned": true, "discountable": true, "parent_scope_id": "CONFINE-job-123-a"},
	} {
		if _, err := validateAdmitArgs(args, admitWaitCeilingMs); err == nil || !strings.Contains(err.Error(), CodeProtocol) {
			t.Fatalf("%s: err=%v, want an %s refusal", name, err, CodeProtocol)
		}
	}
	// The count guard grew by exactly one, to 16: a request carrying all 16 known
	// fields (the three exclusive-only ones empty) is accepted; a 17th field is
	// refused by the count.
	all := scoped(map[string]any{"discountable": true, "cpu": int64(1), "vram": int64(0), "signature": "s",
		"exclusive_holder": "CONFINE-h-1-a", "priority": "high", "max_wait_ms": int64(10),
		"exclusive": false, "reason": "", "parent_scope_id": ""})
	if len(all) != 16 {
		t.Fatalf("fixture has %d fields, want 16", len(all))
	}
	if request, err := validateAdmitArgs(all, admitWaitCeilingMs); err != nil || !request.discountable {
		t.Fatalf("16 known fields: discountable=%v err=%v, want accepted", request.discountable, err)
	}
	all["extra"] = true
	if _, err := validateAdmitArgs(all, admitWaitCeilingMs); err == nil || !strings.Contains(err.Error(), "admit requires slice") {
		t.Fatalf("17 fields: err=%v, want the count guard", err)
	}
}

// verifies: AIRA-324 section 8 tests 1 and 3 — under enforce a discountable job
// whose PIN does not fit the ledger is admitted at its discounted booking while
// the physical reading has room (observe refuses the same job: the discount is
// what admits it), and a discounted job whose booking fits the ledger is held
// when its FULL pin does not fit the real free memory. Mutations: remove the
// physical-fit rule (the second case is granted); judge the ledger on the pin
// (the first case is refused). Applying the rule to undiscounted waiters too is
// an EQUIVALENT mutant, not a gap: their ledger check already implies it
// (available <= ceiling - effectiveCurrent).
func TestPinChargePhysicalFitAndLedgerCharge(t *testing.T) {
	const gi = int64(1) << 30
	t.Run("ledger-tight, physically idle: enforce admits at the charge", func(t *testing.T) {
		for _, mode := range []pinChargeMode{pinChargeObserve, pinChargeEnforce} {
			now := time.Unix(90000, 0)
			current := 10 * gi // real use well below the 50 GiB booked
			server := pinServer(t, mode, 64*gi, &current, &now)
			job := pinWaiter(server, 2, 16*gi+3, now) // pin 16 GiB > 14 GiB of ledger room
			queue := queueOf(server, heldLease(1, 50*gi), job)

			server.evaluateAdmitQueue(queue)

			if mode == pinChargeObserve {
				requireAdmitQueued(t, job)
				continue
			}
			waitAdmitGrant(t, job)
			want := discountedPinCharge(16*gi+3, 80)
			if job.ledgerCharge() != want || job.reserve != 16*gi+3 {
				t.Fatalf("booking=%d reserve=%d, want booking %d and reserve (the pin) unchanged", job.ledgerCharge(), job.reserve, want)
			}
			if queue.outstanding != 50*gi+want {
				t.Fatalf("outstanding=%d, want 50 GiB + the charge %d", queue.outstanding, want)
			}
		}
	})
	t.Run("ledger room, physically full: the full pin must fit", func(t *testing.T) {
		now := time.Unix(90100, 0)
		current := 56 * gi // physical free = 8 GiB
		server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
		job := pinWaiter(server, 2, 10*gi, now) // charge 8 GiB fits the 8 GiB of room; pin 10 GiB does not
		if job.ledgerCharge() != 8*gi {
			t.Fatalf("fixture: charge %d", job.ledgerCharge())
		}
		plain := queuedWaiter(3, 3*gi, now) // undiscounted, within the refused head's 4 GiB budget
		queue := queueOf(server, heldLease(1, 20*gi), job, plain)

		server.evaluateAdmitQueue(queue)

		requireAdmitQueued(t, job)
		waitAdmitGrant(t, plain)
		if job.lastGrantable == nil || *job.lastGrantable != 8*gi {
			t.Fatalf("grantable=%v, want 8 GiB in pin bytes (the physical room)", job.lastGrantable)
		}

		// Real use falls: the full pin now fits, and the job is granted.
		current = 40 * gi
		server.evaluateAdmitQueue(queue)
		waitAdmitGrant(t, job)
	})
	t.Run("the boundary: a pin exactly equal to the physical room is granted", func(t *testing.T) {
		// Mutation: `reserve >= physicalFree` (an off-by-one) holds this job.
		now := time.Unix(90150, 0)
		current := 54 * gi // physical free = exactly 10 GiB
		server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
		job := pinWaiter(server, 2, 10*gi, now)
		queue := queueOf(server, heldLease(1, 20*gi), job)
		server.evaluateAdmitQueue(queue)
		waitAdmitGrant(t, job)
	})
	t.Run("ledger-bound refusal: grantable is stated in pin bytes", func(t *testing.T) {
		// Ledger room 6 GiB (58 GiB booked), physical room 24 GiB: the 8 GiB
		// booking does not fit, and the largest PIN whose booking fits 6 GiB is
		// 6 / 0.8 = 7.5 GiB (verified below: that pin books exactly 6 GiB and one
		// byte more does not fit). Mutations: grantable left as the raw ledger
		// room (6 GiB, a booking figure beside a pin Required); the requested
		// job's own discount added to the room (8 GiB, Sol build r1 #3).
		now := time.Unix(90170, 0)
		current := 40 * gi
		server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
		job := pinWaiter(server, 2, 10*gi, now)
		queue := queueOf(server, heldLease(1, 58*gi), job)
		server.evaluateAdmitQueue(queue)
		requireAdmitQueued(t, job)
		want := 6 * gi * 100 / 80
		if job.lastGrantable == nil || *job.lastGrantable != want {
			t.Fatalf("grantable=%v, want %d (the largest pin whose booking fits 6 GiB)", job.lastGrantable, want)
		}
		if discountedPinCharge(want, 80) != 6*gi || discountedPinCharge(want+1, 80) <= 6*gi {
			t.Fatalf("fixture: %d books %d and %d books %d", want, discountedPinCharge(want, 80), want+1, discountedPinCharge(want+1, 80))
		}
	})
	t.Run("ci-shim never applies the rule or the discount", func(t *testing.T) {
		now := time.Unix(90200, 0)
		current := 63 * gi // a host-wide reading shim mode must ignore
		server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
		server.confineMode = runner.ConfineModeShim
		job := pinWaiter(server, 2, 10*gi, now)
		if job.ledgerCharge() != 10*gi {
			t.Fatalf("shim booking=%d, want the pin", job.ledgerCharge())
		}
		queue := queueOf(server, heldLease(1, 20*gi), job)
		server.evaluateAdmitQueue(queue)
		waitAdmitGrant(t, job)
	})
}

// verifies: AIRA-324 section 8 test 6 — head eligibility is judged on the PIN: a
// discounted waiter whose pin is above the ceiling is never a head even when its
// charge fits, pushed or not, so a smaller job behind it is granted; once the
// ceiling recovers it is a (pushed) head again. Mutation: charge-only
// eligibility (admitCouldFitEmptyLocked reads ledgerCharge) -> the pushed
// discounted waiter becomes a head nobody can pass and the small job is frozen.
func TestPinChargeHeadEligibilityIsOnThePin(t *testing.T) {
	const gi = int64(1) << 30
	for _, pushed := range []bool{true, false} {
		t.Run(fmt.Sprintf("pushed=%t", pushed), func(t *testing.T) {
			now := time.Unix(91000, 0)
			current := int64(0)
			server := pinServer(t, pinChargeEnforce, 10*gi, &current, &now)
			big := pinWaiter(server, 2, 11*gi, now) // pin 11 GiB > 10 GiB slice; charge 8.8 GiB fits it
			queue := queueOf(server, big)
			if pushed {
				queue.seq++
				big.pushSeq = queue.seq
			}
			small := queuedWaiter(3, 1*gi, now)
			queue.waiters = append(queue.waiters, small)

			server.evaluateAdmitQueue(queue)

			requireAdmitQueued(t, big)
			waitAdmitGrant(t, small)
			if queue.freezeHolderSeq != 0 {
				t.Fatalf("freezeHolderSeq=%d: the unfittable pin was made a head", queue.freezeHolderSeq)
			}
		})
	}
}

// verifies: AIRA-324 section 8 test 6 — the AIRA-307 bypass budget is half the
// head's BOOKING and accumulates in booking bytes over several passes. Head: pin
// 20 GiB, charge 16 GiB, budget 8 GiB. Discounted jumpers of pin 5 GiB (charge
// 4 GiB) pass one per pass: after two passes bypassed is 8 GiB and the third is
// withheld. Mutations: budget from the pin (10 GiB: the third, a 1 GiB job, is
// granted); accumulate the jumper's pin (5+5 > 8: the second is withheld).
func TestPinChargeBypassBudgetIsInChargeBytes(t *testing.T) {
	const gi = int64(1) << 30
	now := time.Unix(92000, 0)
	current := int64(0)
	server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
	head := pinWaiter(server, 2, 20*gi, now)
	if head.ledgerCharge() != 16*gi || admitBypassBudget(head) != 8*gi {
		t.Fatalf("fixture: head charge %d budget %d", head.ledgerCharge(), admitBypassBudget(head))
	}
	held := heldLease(1, 50*gi) // room 14 GiB < the head's 16 GiB charge
	queue := queueOf(server, held, head)

	a := pinWaiter(server, 3, 5*gi, now)
	queue.waiters = append(queue.waiters, a)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, a)
	if head.bypassed != 4*gi {
		t.Fatalf("after pass 1 bypassed=%d, want 4 GiB (the jumper's charge)", head.bypassed)
	}

	b := pinWaiter(server, 4, 5*gi, now)
	queue.waiters = append(queue.waiters, b)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, b)
	if head.bypassed != 8*gi {
		t.Fatalf("after pass 2 bypassed=%d, want 8 GiB", head.bypassed)
	}

	c := queuedWaiter(5, 1*gi, now)
	queue.waiters = append(queue.waiters, c)
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, c)
	requireAdmitQueued(t, head)
}

// pinGrantFrame drives one admit request through the real admitConnection and
// returns the grant frame's raw Data, with the queue's evaluator granting it.
func pinGrantFrame(t *testing.T, server *Server, args map[string]any) (json.RawMessage, *sliceQueue) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, args)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		<-done
		server.retireAdmitQueue()
	})
	frames := make(chan ResponseFrame, 1)
	go func() {
		var frame ResponseFrame
		if err := readFrame(clientConn, &frame); err == nil {
			frames <- frame
		}
	}()
	select {
	case frame := <-frames:
		if !frame.OK {
			t.Fatalf("admit refused: %s %s", frame.Code, frame.Error)
		}
		server.admitRegistryMu.Lock()
		queue := server.admitQueue
		server.admitRegistryMu.Unlock()
		return frame.Data, queue
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("no grant frame")
	}
	return nil, nil
}

// verifies: AIRA-324 section 8 test 1 — through the real admit handler, a
// discountable enforce grant echoes the PIN as `reserve` (the client writes it as
// memory.max) and the booking as `charge`; the ledger holds the charge. An
// observe grant carries no `charge` key at all. Mutations: the grant echoes the
// charge as reserve; charge sent when not discounted; charge omitted.
func TestPinChargeGrantEchoesThePinAndTheCharge(t *testing.T) {
	const pin = int64(4<<30) + 3
	for _, mode := range []pinChargeMode{pinChargeObserve, pinChargeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			now := time.Unix(93000, 0)
			current := int64(0)
			server := pinServer(t, mode, 64<<30, &current, &now)
			server.SetRestartFreezeForTest(0)
			server.admitPollInterval = 5 * time.Millisecond
			data, queue := pinGrantFrame(t, server, map[string]any{
				"slice": "aira.slice", "reserve": pin, "pinned": true, "discountable": true,
				"scope_id": "CONFINE-job-123-a", "name": "job", "owner": runner.ConfineUnknownOwner,
			})
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			var grant AdmitResponse
			_ = json.Unmarshal(data, &grant)
			if grant.Reserve != pin {
				t.Fatalf("grant reserve=%d, want the pin %d (the cap)", grant.Reserve, pin)
			}
			queue.mu.Lock()
			outstanding := queue.outstanding
			queue.mu.Unlock()
			if mode == pinChargeObserve {
				if _, present := raw["charge"]; present {
					t.Fatalf("observe grant carries charge: %s", data)
				}
				if outstanding != pin {
					t.Fatalf("observe outstanding=%d, want the pin", outstanding)
				}
				return
			}
			want := discountedPinCharge(pin, pinChargeFactorDefault)
			if grant.Charge != want || outstanding != want {
				t.Fatalf("enforce: grant charge=%d outstanding=%d, want %d", grant.Charge, outstanding, want)
			}
		})
	}
}

// verifies: AIRA-324 section 8 test 5 — `confine --resize` is judged against and
// writes the BOOKING; the granted reserve (the pin) is untouched. Mutations: judge
// "smaller" against the reserve (a 9 GiB resize of a 10 GiB pin booked at 8 GiB
// is accepted); write the reserve (the pin moves).
func TestPinChargeResizeIsAgainstTheCharge(t *testing.T) {
	const gi = int64(1) << 30
	now := time.Unix(94000, 0)
	current := int64(0)
	server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
	lease := pinWaiter(server, 1, 10*gi, now)
	lease.state, lease.accounted = admitGranted, true
	lease.scopeID, lease.name, lease.owner = "CONFINE-job-123-a", "job", ""
	queue := queueOf(server, lease)
	usage := func(string) (int64, bool, string) { return 1 * gi, true, "" }

	if _, _, err := resizeRunningConfineLocked(queue, lease.scopeID, 9*gi, runner.ConfineUnknownOwner, true, usage); err == nil || !strings.Contains(err.Error(), runner.CodeConfineResizeNotReduction) {
		t.Fatalf("resize to 9 GiB of a job booked at 8 GiB: err=%v, want %s", err, runner.CodeConfineResizeNotReduction)
	}
	result, _, err := resizeRunningConfineLocked(queue, lease.scopeID, 6*gi, runner.ConfineUnknownOwner, true, usage)
	if err != nil {
		t.Fatal(err)
	}
	if result.OldReserveBytes != 8*gi || result.NewReserveBytes != 6*gi {
		t.Fatalf("result=%+v, want old 8 GiB (the booking) new 6 GiB", result)
	}
	if lease.reserve != 10*gi || lease.ledgerCharge() != 6*gi || queue.outstanding != 6*gi {
		t.Fatalf("reserve=%d booking=%d outstanding=%d, want the pin kept and the booking 6 GiB", lease.reserve, lease.ledgerCharge(), queue.outstanding)
	}
}

// verifies: AIRA-324 section 8 test 5 — the re-declare lifecycle with the frozen
// ARDR record (scope id and the GRANTED reserve, no eligibility):
//   - a reconnect to this live daemon re-anchors the lease and books the
//     grant-time discounted charge (not the pin; a --resize is dropped, as it was
//     before AIRA-324);
//   - after a restart (absent lease) the re-declared lease is booked the FULL pin
//     (not f(pin), not the resized value), and a restart under load leaves a
//     signed, possibly negative, available that recovers on release;
//   - a mode change leaves a granted charge alone.
//
// Mutations: the SET books the re-declared reserve (the reconnect case reads the
// pin); the SET keeps a resized charge; establish-granted applies the discount;
// ledgerCharge consults the server's mode (the mode-change case moves).
func TestPinChargeReDeclareLifecycle(t *testing.T) {
	const gi = int64(1) << 30
	t.Run("reconnect to a live daemon keeps the grant-time charge", func(t *testing.T) {
		now := time.Unix(95000, 0)
		current := int64(0)
		server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
		server.admitManualEvaluation = true
		request := admitRequest{scopeID: "CONFINE-job-123-a", name: "job", owner: runner.ConfineUnknownOwner, discountable: true, peerSameUID: true, cpu: 1}
		queue, waiter, _, err := server.enqueueResolvedConfineAdmit("/slice", 10*gi, "pinned:client", 64*gi, request)
		if err != nil {
			t.Fatal(err)
		}
		queue.mu.Lock()
		waiter.state, waiter.accounted = admitGranted, true
		close(waiter.grantedCh)
		queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
		queue.mu.Unlock()
		if waiter.ledgerCharge() != 8*gi {
			t.Fatalf("fixture: booking %d", waiter.ledgerCharge())
		}
		usage := func(string) (int64, bool, string) { return 1 * gi, true, "" }
		queue.mu.Lock()
		_, _, err = resizeRunningConfineLocked(queue, waiter.scopeID, 5*gi, runner.ConfineUnknownOwner, true, usage)
		queue.mu.Unlock()
		if err != nil || waiter.ledgerCharge() != 5*gi {
			t.Fatalf("resize: %v booking %d", err, waiter.ledgerCharge())
		}
		// The keeper re-declares the GRANTED reserve, the pin, with no eligibility.
		_, again, _, err := server.enqueueReDeclare("/slice", 10*gi, "redeclare", admitRequest{scopeID: waiter.scopeID, peerSameUID: true, cpu: 1})
		if err != nil || again != waiter {
			t.Fatalf("re-declare: %v same=%v", err, again == waiter)
		}
		queue.mu.Lock()
		defer queue.mu.Unlock()
		if waiter.reserve != 10*gi || waiter.ledgerCharge() != 8*gi || queue.outstanding != 8*gi {
			t.Fatalf("after reconnect reserve=%d booking=%d outstanding=%d, want the pin and the grant-time 8 GiB charge", waiter.reserve, waiter.ledgerCharge(), queue.outstanding)
		}
	})
	t.Run("restart re-declare books the full pin, signed, and recovers", func(t *testing.T) {
		now := time.Unix(95100, 0)
		current := int64(0)
		server := pinServer(t, pinChargeEnforce, 20*gi, &current, &now)
		server.admitManualEvaluation = true
		var leases []*admitWaiter
		for i, id := range []string{"CONFINE-a-1-a", "CONFINE-b-2-a", "CONFINE-c-3-a"} {
			_, lease, _, err := server.enqueueReDeclare("/slice", 8*gi+int64(i), "redeclare", admitRequest{scopeID: id, peerSameUID: true, cpu: 1})
			if err != nil {
				t.Fatal(err)
			}
			leases = append(leases, lease)
		}
		queue := server.admitQueue
		queue.mu.Lock()
		outstanding := queue.outstanding
		available := ledgerAvailable(20*gi, outstanding, 0)
		queue.mu.Unlock()
		if outstanding != 24*gi+3 {
			t.Fatalf("outstanding=%d, want the sum of the full pins (24 GiB + 3)", outstanding)
		}
		if available >= 0 {
			t.Fatalf("available=%d, want negative (signed; over the 20 GiB ceiling)", available)
		}
		for _, lease := range leases {
			if lease.discountable || lease.ledgerCharge() != lease.reserve {
				t.Fatalf("lease %s discountable=%v booking=%d reserve=%d", lease.scopeID, lease.discountable, lease.ledgerCharge(), lease.reserve)
			}
		}
		queue.mu.Lock()
		if !releaseAdmitWaiterLocked(queue, leases[0]) {
			queue.mu.Unlock()
			t.Fatal("release")
		}
		recovered := ledgerAvailable(20*gi, queue.outstanding, 0)
		queue.mu.Unlock()
		if recovered != 20*gi-(16*gi+3) {
			t.Fatalf("available after one release=%d", recovered)
		}
	})
	t.Run("a mode change leaves a granted charge alone", func(t *testing.T) {
		now := time.Unix(95200, 0)
		current := int64(0)
		server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
		lease := pinWaiter(server, 1, 10*gi, now)
		lease.state, lease.accounted = admitGranted, true
		queue := queueOf(server, lease)
		server.pinChargeMode, server.pinChargePercent = pinChargeOff, 100
		queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
		if queue.outstanding != 8*gi {
			t.Fatalf("outstanding=%d after a mode change, want the granted 8 GiB charge", queue.outstanding)
		}
	})
}

// verifies: AIRA-324 snapshot sites — queued-ahead bytes are BOOKINGS, the
// matched waiter's own figure is its PIN (its consumer compares it with the
// ceiling and with the reserve the client sent), and a granted lease booked below
// its pin publishes the pin beside the booking, which equals scopeReserves.
// Mutations: queued-ahead in pin bytes; queuedReserveBytes as the booking; pins
// published for an undiscounted lease or not at all.
func TestPinChargeSnapshotShowsBookingAndPin(t *testing.T) {
	const gi = int64(1) << 30
	now := time.Unix(96000, 0)
	current := int64(0)
	server := pinServer(t, pinChargeEnforce, 64*gi, &current, &now)
	granted := pinWaiter(server, 1, 10*gi, now)
	granted.state, granted.accounted = admitGranted, true
	plain := heldLease(2, 3*gi)
	plain.scopeID = "CONFINE-plain-2-a"
	ahead := pinWaiter(server, 3, 5*gi, now)
	me := pinWaiter(server, 4, 6*gi+5, now)
	queue := queueOf(server, granted, plain, ahead, me)
	registerAdmitQueue(server, queue)

	snapshot := server.admitSliceSnapshotFor("/slice", me.scopeID)

	if snapshot.queuedAheadBytes != 4*gi {
		t.Fatalf("queuedAheadBytes=%d, want the 4 GiB booking of the waiter ahead", snapshot.queuedAheadBytes)
	}
	if snapshot.queuedReserveBytes != 6*gi+5 {
		t.Fatalf("queuedReserveBytes=%d, want the pin", snapshot.queuedReserveBytes)
	}
	if got := snapshot.scopeReserves[granted.scopeID]; got != 8*gi {
		t.Fatalf("scopeReserves=%d, want the 8 GiB booking", got)
	}
	if !reflect.DeepEqual(snapshot.scopePins, map[string]int64{granted.scopeID: 10 * gi}) {
		t.Fatalf("scopePins=%v, want only the discounted lease's pin", snapshot.scopePins)
	}
	if snapshot.residualBytes() != 0 {
		t.Fatalf("residualBytes=%d", snapshot.residualBytes())
	}
}

// pinScenario is one scripted admission sequence: enqueues through the real
// enqueue path, passes under a changing physical reading, a release, and a
// re-declare. It returns everything a client or operator could observe.
type pinScenarioResult struct {
	States      []admitWaiterState
	Bookings    []int64
	Outstanding []int64
	Grants      []string
	Snapshots   []string
}

func runPinScenario(t *testing.T, mode pinChargeMode) pinScenarioResult {
	t.Helper()
	const gi = int64(1) << 30
	now := time.Unix(97000, 0)
	current := int64(0)
	server := pinServer(t, mode, 32*gi, &current, &now)
	server.admitManualEvaluation = true
	var waiters []*admitWaiter
	var result pinScenarioResult
	observe := func() {
		queue := server.admitQueue
		queue.mu.Lock()
		result.Outstanding = append(result.Outstanding, queue.outstanding)
		for _, w := range waiters {
			result.States = append(result.States, w.state)
			result.Bookings = append(result.Bookings, w.ledgerCharge())
			if w.state == admitGranted {
				grant := AdmitResponse{State: w.outcome, Reserve: w.reserve, Basis: w.basis}
				if w.discounted() {
					grant.Charge = w.ledgerCharge()
				}
				data, _ := json.Marshal(grant)
				result.Grants = append(result.Grants, string(data))
			}
		}
		queue.mu.Unlock()
		snap := server.admitSliceSnapshotFor("/slice", waiters[len(waiters)-1].scopeID)
		result.Snapshots = append(result.Snapshots, fmt.Sprintf("%d %d %d %v %v", snap.outstanding, snap.queuedAheadBytes, snap.queuedReserveBytes, snap.scopeReserves, snap.scopePins))
	}
	enqueue := func(i int, pin int64, discountable bool) {
		request := admitRequest{scopeID: fmt.Sprintf("CONFINE-j%d-%d-a", i, 100+i), name: fmt.Sprintf("j%d", i), owner: runner.ConfineUnknownOwner, discountable: discountable, pinned: true, peerSameUID: true, cpu: 1}
		_, w, _, err := server.enqueueResolvedConfineAdmit("/slice", pin, "pinned:client", 32*gi, request)
		if err != nil {
			t.Fatal(err)
		}
		waiters = append(waiters, w)
	}
	pass := func() {
		server.evaluateAdmitQueue(server.admitQueue)
		observe()
	}
	enqueue(0, 12*gi+1, true)
	enqueue(1, 12*gi+3, true)
	enqueue(2, 9*gi, false)
	enqueue(3, 6*gi+7, true)
	pass()
	current = 20 * gi
	pass()
	current = 5 * gi
	pass()
	queue := server.admitQueue
	queue.mu.Lock()
	releaseAdmitWaiterLocked(queue, waiters[0])
	queue.mu.Unlock()
	pass()
	if _, _, _, err := server.enqueueReDeclare("/slice", waiters[1].reserve, "redeclare", admitRequest{scopeID: waiters[1].scopeID, peerSameUID: true, cpu: 1}); err != nil && waiters[1].state == admitGranted {
		t.Fatal(err)
	}
	enqueue(4, 4*gi+9, true)
	pass()
	return result
}

// verifies: AIRA-324 observe is BYTE-IDENTICAL to off — the same scripted queue
// (enqueues through the real enqueue path, passes under a changing physical
// reading, a release, a re-declare) gives identical states, bookings, ledger
// totals, grant frames and snapshots in off and observe; and enforce DIFFERS on
// the same script, so the comparison is not vacuous. Mutation: observe applies
// the discount (pinChargeFor's mode test widened to observe) -> red.
func TestPinChargeObserveIsByteIdenticalToOff(t *testing.T) {
	off := runPinScenario(t, pinChargeOff)
	observe := runPinScenario(t, pinChargeObserve)
	if !reflect.DeepEqual(off, observe) {
		t.Fatalf("observe differs from off:\noff:     %+v\nobserve: %+v", off, observe)
	}
	enforce := runPinScenario(t, pinChargeEnforce)
	if reflect.DeepEqual(off, enforce) {
		t.Fatal("enforce equals off on the same script: the differential proves nothing")
	}
}

// verifies: AIRA-324 section 5.4 — `confine --list` publishes the booking AND
// the pin for a discounted lease, under different names, and no pin for an
// undiscounted one (absent, never zero, never the booking relabelled).
// Mutations: dropping the confine_manage ApplyConfineScopePins call, or
// publishing the pin for every lease, are red here.
func TestPinChargeConfineListShowsBookingAndPin(t *testing.T) {
	const gi = int64(1) << 30
	slice := t.TempDir()
	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 2 * gi, 64 * gi, 0, true, ""
	}
	discounted := reserveScopeID(t, "pinned", 5201)
	plain := reserveScopeID(t, "plain", 5202)
	reserveScopeDir(t, slice, discounted, 10*gi, gi)
	reserveScopeDir(t, slice, plain, 3*gi, gi)
	queue := &sliceQueue{path: slice, server: server}
	queue.waiters = []*admitWaiter{
		{seq: 1, reserve: 10 * gi, charge: 8 * gi, discountable: true, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), scopeID: discounted, name: "pinned", owner: "session-a"},
		{seq: 2, reserve: 3 * gi, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), scopeID: plain, name: "plain", owner: "session-a"},
	}
	queue.mu.Lock()
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	queue.mu.Unlock()
	registerAdmitQueue(server, queue)

	listing := reserveListing(t, server)
	record := reserveScopeByID(t, listing, discounted)
	if record.ReserveBytes == nil || *record.ReserveBytes != 8*gi {
		t.Fatalf("discounted reserve_bytes=%v, want the 8 GiB booking", record.ReserveBytes)
	}
	if record.PinBytes == nil || *record.PinBytes != 10*gi {
		t.Fatalf("discounted pin_bytes=%v, want the 10 GiB pin", record.PinBytes)
	}
	other := reserveScopeByID(t, listing, plain)
	if other.ReserveBytes == nil || *other.ReserveBytes != 3*gi || other.PinBytes != nil {
		t.Fatalf("plain lease reserve=%v pin=%v, want 3 GiB and NO pin", other.ReserveBytes, other.PinBytes)
	}
}

// verifies: AIRA-324 (Sol build r1 #3) — largestDiscountedPin is the exact
// inverse of discountedPinCharge: for each room, the returned pin books within
// the room and one byte more does not (room of exactly the 1 MiB floor included,
// Sol build r2); a deficit, room below the floor and factor 1
// state the room unchanged.
func TestLargestDiscountedPinInvertsTheCharge(t *testing.T) {
	const gi = int64(1) << 30
	for _, room := range []int64{1 << 20, 2<<20 + 1, 7*gi + 3, 6 * gi, 40*gi + 12345} {
		for _, percent := range []int64{50, 67, 80, 99} {
			pin := largestDiscountedPin(room, percent)
			if discountedPinCharge(pin, percent) > room || discountedPinCharge(pin+1, percent) <= room {
				t.Fatalf("room %d at %d%%: pin %d books %d, pin+1 books %d", room, percent, pin,
					discountedPinCharge(pin, percent), discountedPinCharge(pin+1, percent))
			}
		}
	}
	for _, tc := range []struct{ room, percent int64 }{{-3 * gi, 80}, {0, 80}, {1<<20 - 1, 80}, {5 * gi, 100}} {
		if got := largestDiscountedPin(tc.room, tc.percent); got != tc.room {
			t.Fatalf("largestDiscountedPin(%d, %d) = %d, want the room unchanged", tc.room, tc.percent, got)
		}
	}
}

// verifies: AIRA-324 (Sol build r1 #1) — a grant frame carries `charge` only for
// a DISCOUNTABLE waiter booked below its reserve. A resized lease of a client
// that never sent `discountable` (re-anchored and granted again) must not carry
// one: that client refuses an unasked-for charge and would drop the lease.
// Mutation: echo the charge for every waiter booked below its reserve.
func TestPinChargeGrantChargeOnlyForADiscountableWaiter(t *testing.T) {
	resized := &admitWaiter{reserve: 8 << 30, charge: 2 << 30}
	if got := admitGrantCharge(resized); got != 0 {
		t.Fatalf("a resized, non-discountable lease echoes charge %d, want none", got)
	}
	discounted := &admitWaiter{reserve: 10 << 30, charge: 8 << 30, discountable: true}
	if got := admitGrantCharge(discounted); got != 8<<30 {
		t.Fatalf("discounted grant charge = %d, want the 8 GiB booking", got)
	}
	plain := &admitWaiter{reserve: 10 << 30, discountable: true} // observe: eligible, not discounted
	if got := admitGrantCharge(plain); got != 0 {
		t.Fatalf("an undiscounted eligible grant echoes charge %d, want none", got)
	}
}

// verifies: AIRA-324 (Sol build r1 #2) — confine-dump's waiter row keeps the
// booking in reserve_bytes and adds pin_bytes only for a lease booked below its
// reserve; an ordinary lease has no pin_bytes. Mutations: pin_bytes dropped;
// pin_bytes on every row.
func TestPinChargeDumpShowsBookingAndPin(t *testing.T) {
	server := ciDumpTestServer(t)
	injectCIDumpQueue(server, "/aira.slice", func(queue *sliceQueue) {
		queue.waiters = []*admitWaiter{
			{seq: 1, reserve: 10 << 30, charge: 8 << 30, discountable: true, state: admitGranted, accounted: true, scopeID: "CONFINE-pin-1-a"},
			{seq: 2, reserve: 3 << 30, state: admitGranted, accounted: true, scopeID: "CONFINE-plain-2-a"},
		}
		queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	})
	response := server.confineDump(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	rows := map[string]runner.ConfineDumpWaiterRow{}
	for _, row := range response.Data.(runner.ConfineDumpResult).Waiters {
		rows[row.ScopeID] = row
	}
	pinned, plain := rows["CONFINE-pin-1-a"], rows["CONFINE-plain-2-a"]
	if pinned.ReserveBytes != 8<<30 || pinned.PinBytes == nil || *pinned.PinBytes != 10<<30 {
		t.Fatalf("discounted row reserve=%d pin=%v, want booking 8 GiB and pin 10 GiB", pinned.ReserveBytes, pinned.PinBytes)
	}
	if plain.ReserveBytes != 3<<30 || plain.PinBytes != nil {
		t.Fatalf("plain row reserve=%d pin=%v, want 3 GiB and no pin", plain.ReserveBytes, plain.PinBytes)
	}
}
