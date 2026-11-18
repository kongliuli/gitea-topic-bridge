//go:build !windows

package main

import (
	"context"
	"errors"
)

func runWindowsService(_ string, _ func(context.Context, func()) error) error {
	return errors.New("native Windows service mode requires Windows")
}
