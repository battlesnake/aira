package runner

import "strings"

// AIRA-350. The small-job lane's two sizes, shared by the daemon (which reads
// them from AIRA_DAEMON_SMALL_LANE and AIRA_DAEMON_SMALL_LANE_JOB_MAX) and by
// `aira install` (which renders them from --small-lane and --small-lane-job-max),
// so the two faces cannot accept different values. Spec
// docs/superpowers/specs/2026-11-18-fixed-small-lane-design.md section 2.5.
//
// The lane lets jobs booking at most the job maximum (y) be admitted ahead of
// larger queued jobs while such small jobs hold at most the capacity (x), inside
// the one slice ceiling.
const (
	// SmallLaneOff is the value that turns the lane off.
	SmallLaneOff = "off"
	// SmallLaneInstallDefault and SmallLaneJobMaxInstallDefault are what `aira
	// install` renders when neither the flag nor an installed value says
	// otherwise. The daemon's raw default (no variable set) is off.
	SmallLaneInstallDefault       = "8G"
	SmallLaneJobMaxInstallDefault = "1G"
	// SmallLaneJobMaxDefaultBytes is the job maximum when the lane is on and
	// AIRA_DAEMON_SMALL_LANE_JOB_MAX is unset.
	SmallLaneJobMaxDefaultBytes int64 = 1 << 30
	// SmallLaneJobMaxMinBytes is the smallest job maximum: the smallest pin a
	// confine client accepts (MinPinnedScopeCap).
	SmallLaneJobMaxMinBytes int64 = 1 << 20
	// SmallLaneMaxBytes is the largest capacity: the daemon's largest admissible
	// reserve (admitMaxReserve).
	SmallLaneMaxBytes int64 = 1 << 50
	// The two bounds as sizes, so a face can check one value of the pair
	// alone through ParseSmallLane (a lane against the smallest job maximum, a
	// job maximum against the largest lane).
	SmallLaneJobMaxMinBytesText = "1M"
	SmallLaneMaxBytesText       = "1048576G" // SmallLaneMaxBytes
)

// ParseSmallLane parses the pair. capacityRaw "", or "off", is the lane off
// (capacity 0), and jobMaxRaw is then not read: a typo in the job maximum must
// not refuse to start a daemon that did not ask for the lane (the pin-charge
// ordering). Otherwise capacity is a size from the job maximum to
// SmallLaneMaxBytes, and jobMaxRaw is a size from SmallLaneJobMaxMinBytes to the
// capacity ("" is SmallLaneJobMaxDefaultBytes). problem names the refused value,
// "" when the pair is valid. Nothing is rounded or clamped: a value the operator
// typed and silently did not get would be a fake pass.
func ParseSmallLane(capacityRaw, jobMaxRaw string) (capacity, jobMax int64, problem string) {
	capacityRaw = strings.TrimSpace(capacityRaw)
	if capacityRaw == "" || capacityRaw == SmallLaneOff {
		return 0, 0, ""
	}
	capacity, err := ParseMemorySize(capacityRaw)
	if err != nil || capacity <= 0 || capacity > SmallLaneMaxBytes {
		return 0, 0, "capacity"
	}
	jobMax = SmallLaneJobMaxDefaultBytes
	if raw := strings.TrimSpace(jobMaxRaw); raw != "" {
		parsed, err := ParseMemorySize(raw)
		if err != nil {
			return 0, 0, "job-max"
		}
		jobMax = parsed
	}
	if jobMax < SmallLaneJobMaxMinBytes || jobMax > capacity {
		return 0, 0, "job-max"
	}
	return capacity, jobMax, ""
}
