//go:build !linux

package main

import (
	"errors"

	"github.com/malahmen/clonecast/internal/broadcast"
)

func newLinuxPlatform() (*platform, error) {
	return nil, errors.New("the linux backend only builds on Linux; use --backend mock here")
}

// xsendAvailable: the xsend backend uses X11 and only builds on Linux, so the
// TUI's backend setting must not offer it here.
const xsendAvailable = false

// setupXsend: the xsend backend uses X11 and only builds on Linux.
func setupXsend(_ broadcast.WindowManager, _ string) (broadcast.Deliverer, func(), error) {
	return nil, nil, errors.New("--deliver xsend is only available on Linux")
}
