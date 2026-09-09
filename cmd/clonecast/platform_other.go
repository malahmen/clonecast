//go:build !linux

package main

import (
	"errors"

	"github.com/malahmen/clonecast/internal/broadcast"
)

func newLinuxPlatform() (*platform, error) {
	return nil, errors.New("the linux backend only builds on Linux; use --backend mock here")
}

// setupXsend: the xsend backend uses X11 and only builds on Linux.
func setupXsend(_ *broadcast.Engine, _ broadcast.WindowManager, _ string) (func(), error) {
	return nil, errors.New("--deliver xsend is only available on Linux")
}
