//go:build !windows && !linux && !darwin

package bridge

import "os"

func openArtifact(_ string, _ []string) (*os.File, error) {
	return nil, errUnsafeArtifact
}
