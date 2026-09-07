//go:build !linux

package main

import "errors"

func newLinuxPlatform() (*platform, error) {
	return nil, errors.New("the linux backend only builds on Linux; use --backend mock here")
}
