//go:build linux

package evdev

import (
	"errors"
	"strings"
	"sync"
	"syscall"

	ev "github.com/holoplot/go-evdev"
)

// Clicks watches every mouse for button presses, without grabbing: the
// compositor and the games still get every click. It is how the focus guard
// knows the user clicked, which a game warping the pointer onto itself cannot
// fake (REFERENCE.md 4.24).
type Clicks struct {
	devs []*ev.InputDevice
	once sync.Once
}

// WatchClicks calls clicked on each mouse button press.
func WatchClicks(clicked func()) (*Clicks, error) {
	all, err := ev.ListDevicePaths()
	if err != nil {
		return nil, err
	}
	c := &Clicks{}
	for _, p := range all {
		if strings.Contains(p.Name, VirtualName) {
			continue
		}
		d, err := ev.OpenWithFlags(p.Path, syscall.O_RDONLY)
		if err != nil {
			continue
		}
		if !hasKey(d, ev.BTN_LEFT) {
			_ = d.Close()
			continue
		}
		c.devs = append(c.devs, d)
		go read(d, clicked)
	}
	if len(c.devs) == 0 {
		return nil, errors.New("no mouse found under /dev/input")
	}
	return c, nil
}

func read(d *ev.InputDevice, clicked func()) {
	for {
		e, err := d.ReadOne()
		if err != nil {
			return // closed or unplugged
		}
		if e.Type == ev.EV_KEY && e.Value == 1 && e.Code >= ev.BTN_LEFT && e.Code <= ev.BTN_TASK {
			clicked()
		}
	}
}

// Close stops watching.
func (c *Clicks) Close() error {
	c.once.Do(func() {
		for _, d := range c.devs {
			_ = d.Close()
		}
	})
	return nil
}
