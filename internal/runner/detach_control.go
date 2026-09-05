package runner

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// writeControlValue and consumeControlValue are the ONE detach control-file
// primitive. `run --detach` (LaunchDetached) and `confine --detach`
// (LaunchConfineDetached) both hand a request to a setsid'd shim through a file
// rather than through argv, and both need exactly the same properties: the file
// is owner-only, durable before the shim is spawned, consumed exactly once, and
// strictly decoded so a schema drift between the launching binary and the shim
// binary is a refusal rather than a silently half-applied request.
//
// AIRA-22 generalised these from Request-specific to any value. The behaviour is
// unchanged for the run path -- writeDetachControl/consumeDetachControl are now
// one-line wrappers, so the existing M20 tests keep proving the contract they
// always proved.
func writeControlValue(dir, pattern string, value any) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(value); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	keep = true
	return path, nil
}

// consumeControlValue reads and REMOVES the control file, then decodes it into
// into. Removal precedes decoding deliberately: a control file that outlived its
// single consumer would let a second shim launch the same request.
func consumeControlValue(path string, into any) error {
	data, readErr := os.ReadFile(path)
	removeErr := os.Remove(path)
	if removeErr == nil {
		removeErr = syncDir(filepath.Dir(path))
	}
	if readErr != nil {
		return readErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	dec := json.NewDecoder(bytesReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("control file contains trailing data")
	}
	return nil
}

func writeDetachControl(outputDir string, req Request) (string, error) {
	return writeControlValue(outputDir, "detach-*.ctrl", req)
}

func consumeDetachControl(path string) (Request, error) {
	var req Request
	if err := consumeControlValue(path, &req); err != nil {
		return Request{}, err
	}
	return req, nil
}

func ConsumeDetachControl(path string) (Request, error) { return consumeDetachControl(path) }
