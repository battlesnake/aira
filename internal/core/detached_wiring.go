package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"aira/internal/store"
)

const detachedWiringMaxBytes int64 = 1 << 20

// renameWiringFile is the atomic-publish seam. Tests inject a failure to prove the
// final sidecar only ever appears via the rename (never a partial direct write).
var renameWiringFile = os.Rename

type detachedWiringSidecar struct {
	Schema        int                         `json:"schema"`
	Params        WiringParams                `json:"params"`
	ReportContext detachedReportContextOnWire `json:"report_context"`
}

type detachedReportContextOnWire struct {
	Commit     string `json:"commit"`
	Branch     string `json:"branch"`
	WorktreeID string `json:"worktree_id"`
}

func writeDetachedWiringSidecar(outputDir string, params WiringParams, reportContext store.TestReportContext) (string, error) {
	if strings.TrimSpace(outputDir) == "" {
		return "", errors.New("detached wiring output directory is required")
	}
	abs, err := filepath.Abs(outputDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	wire := detachedWiringSidecar{Schema: 1, Params: cloneWiringParams(params), ReportContext: detachedReportContextOnWire{
		Commit: reportContext.Commit, Branch: reportContext.Branch, WorktreeID: reportContext.WorktreeID,
	}}
	payload, err := json.Marshal(wire)
	if err != nil {
		return "", err
	}
	if int64(len(payload)) > detachedWiringMaxBytes {
		return "", errors.New("detached wiring sidecar is too large")
	}
	tmp, err := os.CreateTemp(abs, ".detach-*.wiring.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	finalPath := strings.TrimSuffix(tmpPath, ".tmp")
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
			_ = os.Remove(finalPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := tmp.Write(payload); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := renameWiringFile(tmpPath, finalPath); err != nil {
		return "", err
	}
	if err := syncWiringDir(abs); err != nil {
		return "", err
	}
	keep = true
	return finalPath, nil
}

// ConsumeDetachedWiringSidecar reads, removes, then validates the launch-window
// sidecar. Removal occurs before decoding so malformed inputs do not persist.
func ConsumeDetachedWiringSidecar(path string) (WiringParams, store.TestReportContext, error) {
	if !filepath.IsAbs(path) {
		return WiringParams{}, store.TestReportContext{}, errors.New("detached wiring sidecar path must be absolute")
	}
	f, err := os.Open(path)
	if err != nil {
		return WiringParams{}, store.TestReportContext{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, detachedWiringMaxBytes+1))
	closeErr := f.Close()
	removeErr := os.Remove(path)
	if removeErr == nil {
		removeErr = syncWiringDir(filepath.Dir(path))
	}
	if readErr != nil {
		return WiringParams{}, store.TestReportContext{}, readErr
	}
	if closeErr != nil {
		return WiringParams{}, store.TestReportContext{}, closeErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return WiringParams{}, store.TestReportContext{}, removeErr
	}
	if int64(len(data)) > detachedWiringMaxBytes {
		return WiringParams{}, store.TestReportContext{}, errors.New("detached wiring sidecar exceeds size limit")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var wire detachedWiringSidecar
	if err := dec.Decode(&wire); err != nil {
		return WiringParams{}, store.TestReportContext{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return WiringParams{}, store.TestReportContext{}, errors.New("detached wiring sidecar contains trailing data")
	}
	if wire.Schema != 1 {
		return WiringParams{}, store.TestReportContext{}, fmt.Errorf("unsupported detached wiring schema %d", wire.Schema)
	}
	params := cloneWiringParams(wire.Params)
	return params, store.TestReportContext{Commit: wire.ReportContext.Commit, Branch: wire.ReportContext.Branch, WorktreeID: wire.ReportContext.WorktreeID}, nil
}

func removeDetachedWiringSidecar(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncWiringDir(filepath.Dir(path))
}

func syncWiringDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
