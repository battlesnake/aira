//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// stubConfineImpl swaps the funnel's launch seam for a canned outcome and
// reports whether it was ever called.
func stubConfineImpl(t *testing.T, result ConfineResult, err error) *int {
	t.Helper()
	original := confineImpl
	t.Cleanup(func() { confineImpl = original })
	calls := new(int)
	confineImpl = func(context.Context, ConfineRequest) (ConfineResult, error) {
		*calls++
		return result, err
	}
	return calls
}

func readSummaryLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != '\n' {
		t.Fatalf("summary file does not end in a newline: %q", data)
	}
	return bytes.SplitAfter(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
}

// verifies: AIRA-281 -- an unopenable or non-regular --summary-file refuses the
// launch BEFORE the launch implementation runs (no admission, no child, no daemon
// contact), with the stable code, and the never-ran trailer names the slice that
// was attempted rather than slice=unevaluated.
//
// Mutations: opening at the end (the seam is called); dropping O_NONBLOCK (the
// FIFO-without-reader case blocks until the 2 s timeout); dropping the
// regular-file check (the FIFO with a reader is accepted); omitting the
// attempted-slice assignment (slice=unevaluated).
func TestConfineFunnelRefusesBadSummaryPathBeforeLaunch(t *testing.T) {
	dir := t.TempDir()
	fifoNoReader := filepath.Join(dir, "fifo-no-reader")
	fifoReader := filepath.Join(dir, "fifo-reader")
	for _, fifo := range []string{fifoNoReader, fifoReader} {
		if err := unix.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := unix.Open(fifoReader, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(reader)

	for _, tc := range []struct{ name, path, slice, wantSlice string }{
		{"missing directory", filepath.Join(dir, "no", "such", "dir", "out.jsonl"), "my.slice", "my.slice"},
		{"a directory", dir, "my.slice", "my.slice"},
		{"fifo without a reader", fifoNoReader, "", DefaultConfineSlice},
		{"fifo with a reader", fifoReader, "my.slice", "my.slice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AIRA_CONFINE_SLICE", "")
			calls := stubConfineImpl(t, ConfineResult{}, nil)
			var stderr bytes.Buffer
			type outcome struct {
				result ConfineResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := Confine(context.Background(), ConfineRequest{
					Slice: tc.slice, Argv: []string{"true"}, SummaryFile: tc.path, Stderr: &stderr,
				})
				done <- outcome{result, err}
			}()
			var got outcome
			select {
			case got = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Confine blocked on the summary path (a FIFO open must not wait for a reader)")
			}
			if got.err == nil || confineErrorCode(got.err) != "E_CONFINE_ARGUMENT_INVALID" {
				t.Fatalf("err = %v, want E_CONFINE_ARGUMENT_INVALID", got.err)
			}
			if !strings.Contains(got.err.Error(), "--summary-file "+tc.path) {
				t.Fatalf("error does not name the option and path: %v", got.err)
			}
			if *calls != 0 {
				t.Fatalf("the launch implementation ran %d times despite the refused summary path", *calls)
			}
			trailer := stderr.String()
			if !strings.Contains(trailer, ConfineNeverRanFacet+" code=E_CONFINE_ARGUMENT_INVALID slice="+tc.wantSlice+" ") {
				t.Fatalf("never-ran trailer = %q, want slice=%s", trailer, tc.wantSlice)
			}
			if strings.Contains(trailer, "slice=unevaluated") {
				t.Fatalf("the attempted slice was not recorded: %q", trailer)
			}
		})
	}
}

// verifies: AIRA-281 -- the runner refuses a relative path itself (so a detached
// supervisor can never resolve it against its own cwd), and a tree hash with no
// summary file; neither reaches the launch implementation. Accepting a relative
// path, or gating the hash check on SummaryFile != "", reds this.
func TestConfineFunnelRefusesRelativePathAndOrphanedTreeHash(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request ConfineRequest
		want    string
	}{
		{"relative path", ConfineRequest{SummaryFile: "out.jsonl"}, "must be absolute"},
		{"orphaned tree hash", ConfineRequest{SummaryTreeHash: "abc123"}, "requires --summary-file"},
		{"invalid tree hash with a file", ConfineRequest{SummaryFile: filepath.Join(t.TempDir(), "o.jsonl"), SummaryTreeHash: "a b"}, "--summary-tree-hash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubConfineImpl(t, ConfineResult{}, nil)
			tc.request.Argv = []string{"true"}
			tc.request.Stderr = &bytes.Buffer{}
			_, err := Confine(context.Background(), tc.request)
			if err == nil || confineErrorCode(err) != "E_CONFINE_ARGUMENT_INVALID" || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want E_CONFINE_ARGUMENT_INVALID containing %q", err, tc.want)
			}
			if *calls != 0 {
				t.Fatal("the launch implementation ran")
			}
			// The refusal creates nothing at the path.
			if tc.request.SummaryFile != "" && filepath.IsAbs(tc.request.SummaryFile) {
				if _, statErr := os.Stat(tc.request.SummaryFile); statErr == nil {
					t.Fatal("a refused launch created the summary file")
				}
			}
		})
	}
}

// verifies: AIRA-281 -- ONE write of the WHOLE line, on the fd that
// openConfineSummaryFile returned (it names the requested file), after the outcome
// is final. Writing body and newline separately reds the count.
func TestConfineFunnelMakesExactlyOneWholeLineWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	stubConfineImpl(t, ConfineResult{Status: ranStatus(), Exit: 7}, nil)
	original := writeConfineSummaryFn
	t.Cleanup(func() { writeConfineSummaryFn = original })
	var (
		mu     sync.Mutex
		writes [][]byte
		target string
	)
	writeConfineSummaryFn = func(fd int, line []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		writes = append(writes, append([]byte(nil), line...))
		target, _ = os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		return original(fd, line)
	}
	var stderr bytes.Buffer
	result, err := Confine(context.Background(), ConfineRequest{Argv: []string{"true"}, SummaryFile: path, Stderr: &stderr})
	if err != nil || result.Exit != 7 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(writes) != 1 {
		t.Fatalf("%d writes, want exactly 1", len(writes))
	}
	if !bytes.HasSuffix(writes[0], []byte("}\n")) || bytes.Count(writes[0], []byte("\n")) != 1 {
		t.Fatalf("the single write is not one whole line: %q", writes[0])
	}
	if target != path {
		t.Fatalf("write went to fd naming %q, want %q", target, path)
	}
	lines := readSummaryLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("file holds %d lines, want 1", len(lines))
	}
	_, values := summaryParse(t, lines[0])
	if values["ran"] != true || values["exit"] != json.Number("7") {
		t.Fatalf("line = %v", values)
	}
	if strings.Contains(stderr.String(), ConfineSummaryUnwrittenFacet) {
		t.Fatalf("a good write reported unwritten: %q", stderr.String())
	}
	// Appending: a second job adds a second line and keeps the first.
	if _, err := Confine(context.Background(), ConfineRequest{Argv: []string{"true"}, SummaryFile: path, Stderr: &stderr}); err != nil {
		t.Fatal(err)
	}
	if got := len(readSummaryLines(t, path)); got != 2 {
		t.Fatalf("file holds %d lines after two jobs, want 2 (append, never truncate)", got)
	}
}

func newTestHelperCommand(path string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfineSummaryConcurrentAppendersDoNotInterleave$")
	cmd.Env = append(os.Environ(), "AIRA_SUMMARY_STRESS_FILE="+path)
	return cmd
}

// verifies: AIRA-281 invariant 4 -- a failed or short write never changes the
// result or the error (Exit 0 stays 0, err stays nil), is reported by the fixed
// token on stderr, and is NEVER retried (a short write is attempted once).
func TestConfineFunnelWriteFailureIsReportedAndChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(fd int, line []byte) (int, error)
		cause string
	}{
		{"EIO", func(int, []byte) (int, error) { return 0, syscall.EIO }, "input/output error"},
		{"short write", func(_ int, line []byte) (int, error) { return len(line) / 2, nil }, "short write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.jsonl")
			stubConfineImpl(t, ConfineResult{Status: ranStatus(), Exit: 0}, nil)
			original := writeConfineSummaryFn
			t.Cleanup(func() { writeConfineSummaryFn = original })
			calls := 0
			writeConfineSummaryFn = func(fd int, line []byte) (int, error) {
				calls++
				return tc.write(fd, line)
			}
			var stderr bytes.Buffer
			result, err := Confine(context.Background(), ConfineRequest{Argv: []string{"true"}, SummaryFile: path, Stderr: &stderr})
			if err != nil {
				t.Fatalf("a failed summary write changed the error: %v", err)
			}
			if result.Exit != 0 {
				t.Fatalf("a failed summary write changed the exit to %d", result.Exit)
			}
			if calls != 1 {
				t.Fatalf("write attempted %d times, want 1 (never retried)", calls)
			}
			text := stderr.String()
			if !strings.Contains(text, "confine: "+ConfineSummaryUnwrittenFacet+" path="+path+" cause=") || !strings.Contains(text, tc.cause) {
				t.Fatalf("stderr = %q, want the fixed unwritten token naming the path and %q", text, tc.cause)
			}
		})
	}
}

// verifies: AIRA-281 -- ran is err == nil, never derived from Exit: a canned
// success writes ran:true with its exit; a canned E_ADMIT_FAILFAST_TRIPPED writes
// ran:false with that code and admission "failfast_tripped" and no ran-only keys.
func TestConfineFunnelWritesRanAndNeverRanLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	// ran:true with a NON-zero exit and ran:false share nothing but the file.
	stubConfineImpl(t, ConfineResult{Status: ranStatus(), Exit: 4}, nil)
	if _, err := Confine(context.Background(), ConfineRequest{Argv: []string{"true"}, SummaryFile: path, Stderr: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	refused := ConfineStatus{Slice: ShimConfineSlice, Name: "leg", Owner: "o", AdmissionState: "failfast_tripped"}
	stubConfineImpl(t, ConfineResult{Status: refused, Exit: 0}, errors.New("E_ADMIT_FAILFAST_TRIPPED: slice tripped"))
	_, err := Confine(context.Background(), ConfineRequest{Argv: []string{"true"}, SummaryFile: path, Stderr: &bytes.Buffer{}})
	if confineErrorCode(err) != "E_ADMIT_FAILFAST_TRIPPED" {
		t.Fatalf("err = %v", err)
	}
	lines := readSummaryLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2", len(lines))
	}
	_, ran := summaryParse(t, lines[0])
	if ran["ran"] != true || ran["exit"] != json.Number("4") || ran["code"] != nil {
		t.Fatalf("ran line = %v", ran)
	}
	keys, never := summaryParse(t, lines[1])
	if never["ran"] != false || never["code"] != "E_ADMIT_FAILFAST_TRIPPED" || never["admission"] != "failfast_tripped" {
		t.Fatalf("never-ran line = %v", never)
	}
	for _, key := range []string{"exit", "terminated_by", "wall_us", "peak_rss_bytes", "rusage_user_us"} {
		if containsKey(keys, key) {
			t.Errorf("never-ran line carries ran-only key %s", key)
		}
	}
}

// verifies: AIRA-281 -- evidence, not a mutation guard (the one-write test is the
// deterministic guard): 32 real processes each append 200 worst-case-sized lines to
// one file through the real O_APPEND open and write; every line parses and the
// count is 6400.
func TestConfineSummaryConcurrentAppendersDoNotInterleave(t *testing.T) {
	if path := os.Getenv("AIRA_SUMMARY_STRESS_FILE"); path != "" {
		// Helper mode: one appender process.
		status := ranStatus()
		status.Name = strings.Repeat("n", 100)
		status.Slice = strings.Repeat("s", 255)
		line, err := FormatConfineSummary(ConfineRequest{Argv: []string{"x"}, SummaryTreeHash: strings.Repeat("t", 128)}, ConfineResult{Status: status}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 200; i++ {
			fd, err := openConfineSummaryFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := appendConfineSummary(fd, line); err != nil {
				t.Fatal(err)
			}
			closeConfineSummaryFile(fd)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "stress.jsonl")
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := newTestHelperCommand(path)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- errors.New(err.Error() + ": " + string(out))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	lines := readSummaryLines(t, path)
	if len(lines) != workers*200 {
		t.Fatalf("%d lines, want %d", len(lines), workers*200)
	}
	for i, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("line %d does not parse (interleaved?): %q", i, line)
		}
	}
}
