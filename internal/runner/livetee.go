package runner

import (
	"fmt"
	"io"
	"sync/atomic"
)

// liveQueueDepth is deliberately small: live output is an observation path,
// while the capture file remains the authoritative record.
var liveQueueDepth = 64

// liveStreamCreateHook is a package-local fault-test seam. Production code
// leaves it nil.
var liveStreamCreateHook func()

const liveMarkerFormat = "[aira: %d bytes elided from live view — see run-log]"

type liveChunk struct {
	data          []byte
	droppedBefore int64
}

type liveGate struct {
	w   io.Writer
	off atomic.Bool
}

func (g *liveGate) write(p []byte) error {
	if g.off.Load() {
		return nil
	}
	return writeAll(g.w, p)
}

func (g *liveGate) disable() {
	g.off.Store(true)
}

type liveStream struct {
	ch           chan liveChunk
	gate         liveGate
	finalDropped int64
	done         chan struct{}
}

func newLiveStream(w io.Writer) *liveStream {
	if liveStreamCreateHook != nil {
		liveStreamCreateHook()
	}
	stream := &liveStream{
		ch:   make(chan liveChunk, liveQueueDepth),
		gate: liveGate{w: w},
		done: make(chan struct{}),
	}
	go stream.writeLoop()
	return stream
}

func (s *liveStream) writeLoop() {
	defer close(s.done)
	off := false
	emit := func(data []byte) {
		if off || s.gate.off.Load() {
			off = true
			return
		}
		if err := s.gate.write(data); err != nil {
			off = true
			s.gate.disable()
		}
	}
	for chunk := range s.ch {
		if chunk.droppedBefore > 0 {
			emit([]byte(liveMarker(chunk.droppedBefore)))
		}
		if len(chunk.data) > 0 {
			emit(chunk.data)
		}
	}
	if s.finalDropped > 0 {
		emit([]byte(liveMarker(s.finalDropped)))
	}
}

func liveMarker(count int64) string {
	return fmt.Sprintf(liveMarkerFormat, count)
}

func writeAll(w io.Writer, data []byte) error {
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
