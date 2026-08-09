package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"aira/internal/core"
)

const skillUsage = "usage: aira skill guide | install <dir> [--force]"

func runSkill(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 1 && strings.ToLower(argv[0]) == "guide" {
		artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
		if err != nil {
			return skillError(stderr, err)
		}
		_, _ = stdout.Write(artifacts.Guide)
		return 0
	}
	if len(argv) >= 2 && strings.ToLower(argv[0]) == "install" {
		if len(argv) > 3 || (len(argv) == 3 && argv[2] != "--force") {
			return skillUsageError(stderr)
		}
		force := len(argv) == 3
		return installSkill(argv[1], force, stdout, stderr)
	}
	return skillUsageError(stderr)
}

func installSkill(dir string, force bool, stdout, stderr io.Writer) int {
	if strings.TrimSpace(dir) == "" {
		return skillUsageError(stderr)
	}
	info, err := os.Stat(dir)
	if err == nil && !info.IsDir() {
		return skillError(stderr, fmt.Errorf("E_SKILL_INSTALL: target is not a directory"))
	}
	if err != nil && !os.IsNotExist(err) {
		return skillError(stderr, fmt.Errorf("E_SKILL_INSTALL: cannot inspect target: %w", err))
	}
	if os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return skillError(stderr, fmt.Errorf("E_SKILL_INSTALL: cannot create target: %w", err))
		}
	}
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		return skillError(stderr, err)
	}
	targets := []struct {
		path string
		data []byte
	}{
		{path: filepath.Join(dir, "SKILL.md"), data: artifacts.SkillMD},
		{path: filepath.Join(dir, "aira.skill.json"), data: artifacts.ManifestJSON},
	}
	for _, target := range targets {
		old, readErr := os.ReadFile(target.path)
		if readErr == nil {
			if !bytes.Equal(old, target.data) && !force {
				return skillError(stderr, fmt.Errorf("E_SKILL_INSTALL: refusing to overwrite differing %s without --force", target.path))
			}
			continue
		}
		if !os.IsNotExist(readErr) {
			return skillError(stderr, fmt.Errorf("E_SKILL_INSTALL: cannot inspect %s: %w", target.path, readErr))
		}
	}
	for _, target := range targets {
		if err := os.WriteFile(target.path, target.data, 0o644); err != nil {
			return skillError(stderr, fmt.Errorf("E_SKILL_INSTALL: cannot write %s: %w", target.path, err))
		}
	}
	_, _ = fmt.Fprintf(stdout, "SKILL.md: %s\naira.skill.json: %s\nversion: %s\n", targets[0].path, targets[1].path, artifacts.Manifest.Version)
	return 0
}

func skillUsageError(stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, skillUsage)
	return 2
}

func skillError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintln(stderr, err)
	return 2
}
