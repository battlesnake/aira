package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"time"
)

// logAdmitHistoryReadFailure (AIRA-332) records why a per-signature history read
// failed, so an admission sized without its own history (basis
// fallback:history-unavailable, or the p90 prior; for example the first one
// after a reboot) can be diagnosed from the journal. It names the signature by a
// short hash, never the argv, which may carry secrets.
func logAdmitHistoryReadFailure(signature string, elapsed time.Duration, ctxErr, err error) {
	sum := sha256.Sum256([]byte(signature))
	cause := "db-error"
	if ctxErr != nil || errors.Is(err, context.DeadlineExceeded) {
		cause = "timeout"
	}
	log.Printf("aira daemon: confine history read failed (%s after %dms, budget %dms, signature sha256:%s); this admission is sized without its own history: %v",
		cause, elapsed.Milliseconds(), admitHistoryTimeout.Milliseconds(), hex.EncodeToString(sum[:6]), err)
}
