package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func (options launchOptions) openProcessLog(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || strings.HasPrefix(filepath.ToSlash(path), "//") {
		return nil, errors.New("-log-file must use an absolute local path")
	}
	protected := []string{options.secretFile}
	database := options.anchorDB
	var evidenceRoots []string
	if database == "" {
		protected = append(protected, options.config)
		config, err := loadConfig(options.config, launchSecrets{})
		if err != nil {
			return nil, err
		}
		database = config.DatabasePath
		if config.Receipts != nil {
			evidenceRoots = []string{config.Receipts.ArtifactRoot, config.Receipts.SnapshotRoot}
			if config.Receipts.Anchor != nil {
				protected = append(protected, config.Receipts.Anchor.CAFile)
			}
		}
	}
	if database != "" {
		protected = append(protected, database, database+"-wal", database+"-shm", database+"-journal")
	}
	target, logInfo, err := resolveLogPath(path)
	if err != nil {
		return nil, err
	}
	if logInfo != nil && !logInfo.Mode().IsRegular() {
		return nil, errors.New("-log-file must be a regular file")
	}
	for _, candidate := range protected {
		if candidate == "" {
			continue
		}
		filePath, info, err := resolveLogPath(candidate)
		if err != nil {
			return nil, err
		}
		if sameLogPath(target, filePath) || logInfo != nil && info != nil && os.SameFile(logInfo, info) {
			return nil, errors.New("-log-file overlaps a configuration, secret, CA or database file")
		}
	}
	for _, root := range evidenceRoots {
		if root == "" {
			continue
		}
		dir, _, err := resolveLogPath(root)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(dir, target)
		if err == nil && filepath.IsLocal(relative) {
			return nil, errors.New("-log-file must be outside the artifact and receipt snapshot directories")
		}
	}
	return os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
}

func resolveLogPath(path string) (string, os.FileInfo, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(absolute)
	if err == nil {
		resolved, err := filepath.EvalSymlinks(absolute)
		return resolved, info, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	// Resolve existing parents of a file to be created, including Windows junctions.
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if errors.Is(err, os.ErrNotExist) {
		return absolute, nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("resolve deployment path: %w", err)
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil, nil
}

func sameLogPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
