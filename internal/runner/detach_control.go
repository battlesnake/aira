package runner

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func writeDetachControl(outputDir string, req Request) (string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(outputDir, "detach-*.ctrl")
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
	if err := enc.Encode(req); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := syncDir(outputDir); err != nil {
		return "", err
	}
	keep = true
	return path, nil
}

func consumeDetachControl(path string) (Request, error) {
	data, readErr := os.ReadFile(path)
	removeErr := os.Remove(path)
	if removeErr == nil {
		removeErr = syncDir(filepath.Dir(path))
	}
	if readErr != nil {
		return Request{}, readErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return Request{}, removeErr
	}
	dec := json.NewDecoder(bytesReader(data))
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Request{}, errors.New("control file contains trailing data")
	}
	return req, nil
}

func ConsumeDetachControl(path string) (Request, error) { return consumeDetachControl(path) }
