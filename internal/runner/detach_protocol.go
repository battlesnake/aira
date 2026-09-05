package runner

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
)

type detachSignal struct {
	mu   sync.Mutex
	file io.WriteCloser
	sent bool
}

type detachReadyMessage struct {
	ID    string `json:"id,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// send writes ONE readiness message and closes the channel. The payload is
// `any` because two detach paths now share this signal: `run --detach` sends a
// detachReadyMessage and `confine --detach` sends a confineDetachReady, which
// carries the resolved slice and cap a confine launcher must report.
func (s *detachSignal) send(message any) error {
	if s == nil {
		return errors.New("detach readiness channel is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent || s.file == nil {
		return io.ErrClosedPipe
	}
	s.sent = true
	err := json.NewEncoder(s.file).Encode(message)
	closeErr := s.file.Close()
	s.file = nil
	if err == nil {
		err = closeErr
	}
	return err
}

func (s *detachSignal) sentAlready() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}
