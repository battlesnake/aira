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

func (s *detachSignal) send(message detachReadyMessage) error {
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
