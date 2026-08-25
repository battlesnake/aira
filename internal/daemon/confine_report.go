package daemon

import (
	"context"
	"fmt"
	"time"

	"aira/internal/core"
)

func (s *Server) confineReport(args map[string]any) core.Response {
	if len(args) < 2 || len(args) > 3 {
		return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report requires signature, oom, and optional peak_rss"}
	}
	for name := range args {
		if name != "signature" && name != "peak_rss" && name != "oom" {
			return core.Response{Code: CodeProtocol, Error: fmt.Sprintf("%s: unexpected confine-report field %q", CodeProtocol, name)}
		}
	}
	signature, ok := args["signature"].(string)
	if !ok || signature == "" {
		return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report signature must be non-empty"}
	}
	oom, ok := args["oom"].(bool)
	if !ok {
		return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report oom must be boolean"}
	}
	var peak *int64
	if raw, exists := args["peak_rss"]; exists {
		value, valid := exactAdmitInt64(raw)
		if !valid || value <= 0 {
			return core.Response{Code: CodeProtocol, Error: CodeProtocol + ": confine-report peak_rss must be positive"}
		}
		peak = &value
	}
	if s.db == nil {
		return core.Response{Code: CodeUnavailable, Error: CodeUnavailable + ": state database is unavailable"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
	defer cancel()
	if err := s.db.RecordConfinePeak(ctx, signature, peak, oom, time.Now()); err != nil {
		return core.Response{Code: CodeInternal, Error: CodeInternal + ": record confine peak: " + err.Error()}
	}
	return core.Response{OK: true, Code: "OK", Data: map[string]any{"recorded": true}}
}
