//go:build linux || darwin

package bridge

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openArtifact(path string, relative []string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errUnsafeArtifact
	}
	handle, err := unix.Open("/", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { unix.Close(handle) }()
	segments := append(strings.Split(strings.Trim(filepath.Clean(path), "/"), "/"), relative...)
	for index, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.Contains(segment, "/") {
			return nil, errUnsafeArtifact
		}
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index != len(segments)-1 {
			flags |= unix.O_DIRECTORY
		}
		child, err := unix.Openat(handle, segment, flags, 0)
		if err != nil {
			return nil, err
		}
		if index == len(segments)-1 {
			var stat unix.Stat_t
			if err := unix.Fstat(child, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
				unix.Close(child)
				return nil, errUnsafeArtifact
			}
			return os.NewFile(uintptr(child), segment), nil
		}
		unix.Close(handle)
		handle = child
	}
	return nil, errUnsafeArtifact
}
