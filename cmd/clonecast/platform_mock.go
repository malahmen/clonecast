package main

import (
	"time"

	"github.com/charmbracelet/log"

	"github.com/malahmen/clonecast/internal/platform/mock"
)

func newMockPlatform() *platform {
	sink := func(s string) { log.Debug(s) }
	src := mock.NewSource(3 * time.Second)
	wm := mock.NewWM()
	wm.Sink = sink
	return &platform{
		src:   src,
		inj:   &mock.Injector{Sink: sink},
		wm:    wm,
		close: func() { _ = src.Close() },
	}
}
