package runner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ConfineSummarySchema is the version of the one-JSON-line-per-job record that
// `aira confine --summary-file` appends (AIRA-281). Consumers ignore unknown
// keys, so adding a key does not bump it; removing a key, renaming one or
// changing what one means does.
const ConfineSummarySchema = 1

// confineSummaryUnevaluated is the one spelling of "AIRA could not measure
// this": the same word every trailer facet uses, never JSON null and never 0.
const confineSummaryUnevaluated = "unevaluated"

// maxConfineSummaryTreeHashLen bounds the caller-supplied opaque tree hash.
const maxConfineSummaryTreeHashLen = 128

// ValidateConfineSummaryTreeHash checks the opaque value a caller attaches with
// --summary-tree-hash. AIRA never computes or interprets it, so the rule is only
// about what can be copied verbatim into one JSON line: non-empty, at most 128
// bytes, and letters, digits and ._:+/=- (hex, base64, base64url and
// sha256:<hex> all fit, and no allowed character needs JSON escaping).
func ValidateConfineSummaryTreeHash(value string) error {
	if value == "" {
		return errors.New("--summary-tree-hash: value is empty (omit the option instead)")
	}
	if len(value) > maxConfineSummaryTreeHashLen {
		return fmt.Errorf("--summary-tree-hash: value is %d bytes, the maximum is %d", len(value), maxConfineSummaryTreeHashLen)
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:+/=-", c) >= 0 {
			continue
		}
		return errors.New("--summary-tree-hash: value may contain only letters, digits and ._:+/=-")
	}
	return nil
}

// confineAdmissionFacet is the ONE derivation of the admission facet shared by
// the ran trailer, the never-ran trailer and the summary line: the wire-level
// AdmissionState when the runner recorded one, else the coarse Admission value,
// else "" (not established; each caller decides how to spell that).
func confineAdmissionFacet(status ConfineStatus) string {
	if status.AdmissionState != "" {
		return status.AdmissionState
	}
	return string(status.Admission)
}

// confineSummaryRecord is the schema-1 record. The struct order IS the key
// order on the wire. A measured value is either its established type or the
// string "unevaluated", so those fields are `any`; a key that cannot apply at
// all (nothing ran, or no tree hash was supplied) is a nil interface or an empty
// string with omitempty, and is therefore omitted rather than written as a
// fabricated zero.
type confineSummaryRecord struct {
	Schema       int    `json:"schema"`
	Ran          bool   `json:"ran"`
	Name         string `json:"name"`
	Owner        string `json:"owner"`
	ArgvSHA256   string `json:"argv_sha256"`
	TreeHash     string `json:"tree_hash,omitempty"`
	Slice        string `json:"slice"`
	Containment  string `json:"containment"`
	Admission    string `json:"admission"`
	ReserveBytes any    `json:"reserve_bytes"`
	Code         any    `json:"code,omitempty"`
	Exit         any    `json:"exit,omitempty"`
	TerminatedBy any    `json:"terminated_by,omitempty"`
	WallUS       any    `json:"wall_us,omitempty"`
	PeakRSSBytes any    `json:"peak_rss_bytes,omitempty"`
	CPUUserUS    any    `json:"cpu_user_us,omitempty"`
	CPUSysUS     any    `json:"cpu_sys_us,omitempty"`
	RusageUserUS any    `json:"rusage_user_us,omitempty"`
	RusageSysUS  any    `json:"rusage_sys_us,omitempty"`
	RusageMaxRSS any    `json:"rusage_maxrss_largest_process_bytes,omitempty"`
}

// summaryString spells an empty string as "unevaluated".
func summaryString(value string) string {
	if value == "" {
		return confineSummaryUnevaluated
	}
	return value
}

// summaryCounter renders a pointer counter: nil is "unevaluated", a pointer to
// zero is the number 0 (a real observation, e.g. an idle subtree's CPU).
func summaryCounter(value *int64) any {
	if value == nil {
		return confineSummaryUnevaluated
	}
	return *value
}

// summaryPositive renders a counter for which 0 is impossible (peak memory, the
// largest process's RSS, wall time, a reserve): nil or <= 0 reads "unevaluated"
// so an unestablished reading is never written as a fabricated zero.
func summaryPositive(value *int64) any {
	if value == nil || *value <= 0 {
		return confineSummaryUnevaluated
	}
	return *value
}

// confineArgvSHA256 hashes the caller's argv (before any container injection),
// joined by NUL, as 64 lowercase hex characters.
func confineArgvSHA256(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	return hex.EncodeToString(sum[:])
}

// FormatConfineSummary renders the schema-1 line for one confine job, ending in
// exactly one "\n". It projects the same ConfineStatus the trailer is rendered
// from, so it cannot drift from it, and it adds only the facts the trailer does
// not carry (ran, code, exit, argv hash, tree hash).
//
// ran is true exactly when err == nil: Confine reserves error returns for "the
// target argv never executed" (AIRA-147), so an error return writes the
// ran:false shape (a `code`, none of the ran-only keys) and a nil error writes
// the ran:true shape (`exit` and `terminated_by`, no `code`).
func FormatConfineSummary(request ConfineRequest, result ConfineResult, err error) ([]byte, error) {
	status := result.Status
	record := confineSummaryRecord{
		Schema:       ConfineSummarySchema,
		Ran:          err == nil,
		Name:         summaryString(status.Name),
		Owner:        summaryString(status.Owner),
		ArgvSHA256:   confineArgvSHA256(request.Argv),
		TreeHash:     request.SummaryTreeHash,
		Slice:        summaryString(status.Slice),
		Containment:  summaryString(string(status.Containment)),
		Admission:    summaryString(confineAdmissionFacet(status)),
		ReserveBytes: confineSummaryUnevaluated,
	}
	if status.ReserveBytes > 0 {
		record.ReserveBytes = status.ReserveBytes
	}
	if err != nil {
		record.Code = summaryString(confineErrorCode(err))
	} else {
		record.Exit = result.Exit
		record.TerminatedBy = summaryString(status.TerminatedBy)
		record.WallUS = summaryPositive(status.WallUS)
		record.PeakRSSBytes = summaryPositive(status.PeakRSS)
		record.CPUUserUS = summaryCounter(status.CPUUser)
		record.CPUSysUS = summaryCounter(status.CPUSys)
		record.RusageUserUS = summaryCounter(status.RusageUserUS)
		record.RusageSysUS = summaryCounter(status.RusageSysUS)
		record.RusageMaxRSS = summaryPositive(status.RusageMaxRSS)
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// "<" and "&" are legal in a name or path-shaped value and must stay
	// readable; the encoder still never emits a raw newline inside a string.
	encoder.SetEscapeHTML(false)
	// Encode terminates the value with exactly one "\n".
	if encodeErr := encoder.Encode(record); encodeErr != nil {
		return nil, encodeErr
	}
	return buffer.Bytes(), nil
}
