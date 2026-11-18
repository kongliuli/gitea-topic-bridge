package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var errUnsafeArtifact = errors.New("artifact is unavailable, unsafe or too large")

type artifactSelection struct {
	RepositoryID int64  `json:"repository_id"`
	Path         string `json:"path"`
}

type artifactSnapshot struct {
	artifactSelection
	Slot   int    `json:"slot"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func artifactParts(name string) ([]string, bool) {
	if len(name) == 0 || len(name) > 300 || !revertPath.MatchString(name) {
		return nil, false
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if len(part) > 100 || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || !filepath.IsLocal(part) {
			return nil, false
		}
	}
	return parts, len(parts) <= 12
}

type contextReader struct {
	context.Context
	source io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.Err(); err != nil {
		return 0, err
	}
	return reader.source.Read(buffer)
}

func copyArtifact(ctx context.Context, input string, output *os.Root, receiptID string, selected artifactSelection, slot int, bytesLeft int64) (artifactSnapshot, error) {
	snapshot := artifactSnapshot{artifactSelection: selected, Slot: slot}
	parts, valid := artifactParts(selected.Path)
	if !valid || selected.RepositoryID < 1 || slot < 1 || slot > 8 || !topicIDPattern.MatchString(receiptID) || bytesLeft < 0 {
		return snapshot, errUnsafeArtifact
	}
	source, err := openArtifact(input, append([]string{strconv.FormatInt(selected.RepositoryID, 10)}, parts...))
	if err != nil {
		return snapshot, errUnsafeArtifact
	}
	defer source.Close()
	stat, err := source.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > bytesLeft {
		return snapshot, errUnsafeArtifact
	}
	name := filepath.Join(receiptID, fmt.Sprintf("%d.blob", slot))
	destination, err := output.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return snapshot, err
	}
	defer destination.Close()
	copied, err := io.Copy(destination, io.LimitReader(contextReader{ctx, source}, bytesLeft+1))
	if err != nil {
		return snapshot, err
	}
	after, err := source.Stat()
	if err != nil || copied > bytesLeft || copied != stat.Size() || after.Size() != stat.Size() || after.ModTime() != stat.ModTime() {
		return snapshot, errUnsafeArtifact
	}
	if err := destination.Sync(); err != nil {
		return snapshot, err
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return snapshot, err
	}
	hash := sha256.New()
	hashed, err := io.Copy(hash, contextReader{ctx, destination})
	if err != nil {
		return snapshot, err
	}
	if hashed != copied {
		return snapshot, errUnsafeArtifact
	}
	snapshot.Size, snapshot.SHA256 = hashed, hex.EncodeToString(hash.Sum(nil))
	return snapshot, nil
}
