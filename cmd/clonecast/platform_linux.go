//go:build linux

package main

import (
	"fmt"
	"time"

	"github.com/malahmen/clonecast/internal/platform/evdev"
	"github.com/malahmen/clonecast/internal/platform/kwin"
)

func newLinuxPlatform() (*platform, error) {
	src, err := evdev.Open()
	if err != nil {
		return nil, fmt.Errorf("keyboard capture: %w", err)
	}
	inj, err := evdev.NewInjector(src.Devices()[0])
	if err != nil {
		_ = src.Close()
		return nil, fmt.Errorf("virtual keyboard: %w", err)
	}
	// Give the compositor time to notice the new input device before the
	// first passthrough event, otherwise the first keystrokes vanish.
	time.Sleep(500 * time.Millisecond)

	wm, err := kwin.New()
	if err != nil {
		_ = inj.Close()
		_ = src.Close()
		return nil, fmt.Errorf("kwin: %w", err)
	}
	return &platform{
		src: src,
		inj: inj,
		wm:  wm,
		close: func() {
			_ = wm.Close()
			_ = inj.Close()
			_ = src.Close()
		},
	}, nil
}
