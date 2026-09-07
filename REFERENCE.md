# clonecast — design reference

Every decision taken so far, with the reasoning, the alternatives that were considered, and what would make us revisit it. Newest entries at the bottom of each section. Dates are when the decision was made.

Format per entry: **Decision.** Why. *Alternatives.* *Revisit if.*

---

## 1. Goal and scope

**1.1 What it does** (2026-09-07)
Broadcast keyboard input to a user-chosen set of running windows, with a user-chosen set of keys (all, or an explicit list), from a lazygit-style TUI.

**1.2 Target platform is Bazzite desktop mode: KDE Plasma 6 on Wayland.** (2026-09-07)
Narrowing to one platform makes the hard problem (per-window input delivery) tractable. Bazzite is Fedora Atomic with a KDE default image, Wayland only, running games through Steam/Proton.
*Alternatives.* Generic Linux: impossible to do well, because the window-control story differs per compositor. X11: Bazzite does not run an X11 session.
*Revisit if.* Someone needs the GNOME image (needs a GNOME Shell extension, GNOME blocks it by default), or game mode (gamescope is a different compositor with no scripting API).

**1.3 XWayland windows are targets like any other.** (2026-09-07)
KWin owns focus for both native and XWayland windows, so the KWin path covers Proton games without an X11 code path.

---

## 2. Language and libraries

**2.1 Go.** (2026-09-07)
- Single static binary. Bazzite is immutable; dropping one file in `~/.local/bin` is the friction-free install.
- Pure-Go libraries exist for every layer (evdev/uinput, DBus, TUI), so `CGO_ENABLED=0` cross-compilation from the macOS dev box works.
- Goroutines and channels fit the pipeline: one reader per keyboard, one engine loop, one UI.
- Same ecosystem as lazygit/lazydocker, the reference UIs.
*Alternatives.* Rust with ratatui, evdev and zbus crates: equally capable, slower to iterate. Rejected on iteration speed, not capability.
*Revisit if.* Never for this project's size.

**2.2 TUI: Bubble Tea + Bubbles + Lip Gloss + Log (Charm).** (2026-09-07)
The Elm-style update loop maps cleanly onto "engine emits notices, UI subscribes". Bubbles gives text input and viewport for free. Log writes structured logs to a file since stdout is the UI.
*Alternatives.* gocui (what lazygit uses): smaller ecosystem, imperative. Huh (Charm forms): not needed yet, candidate for a settings screen. Wish, Glow, Gum: solve other problems.
*Revisit if.* Bubble Tea v2 stabilises with a compelling reason to migrate; we pin v1 now.

**2.3 One input library: holoplot/go-evdev for both capture and injection.** (2026-09-07)
During bootstrap we found go-evdev already implements uinput, including `CloneDevice`, which copies a physical keyboard's capabilities and vendor/product IDs into the virtual device. That is exactly what we want (see 3.2), so bendahl/uinput was dropped before writing any code against it.
*Revisit if.* go-evdev's uinput support proves buggy on real hardware.

**2.4 godbus/dbus for KWin.** (2026-09-07)
Pure Go, mature, supports exporting objects (we need inbound calls, see 3.3).

**2.5 Go 1.24 in go.mod.** (2026-09-07)
Set by dependency requirements during `go get`, not by choice. Local toolchain auto-downloads.

---

## 3. Input pipeline

**3.1 Capture with an evdev grab (`EVIOCGRAB`) on every keyboard.** (2026-09-07)
It is the only capture method that sees every key on Wayland: X11 methods (XInput2 raw events, XRecord) only observe input routed to XWayland, and there is no Wayland protocol for global key observation by ordinary clients.
Consequence: once grabbed, nobody else gets the keys, so clonecast must replay everything it does not consume (see 4.1).
*Alternatives.* libinput or compositor hooks: not exposed to clients. KWin global shortcuts: only for single hotkeys, not a stream.
*Revisit if.* A portal for input capture (the `InputCapture` xdg-desktop-portal) becomes usable for this; today it targets Synergy-style edge crossing, not full capture.

**3.2 Inject through a uinput virtual keyboard cloned from the first physical keyboard.** (2026-09-07)
A cloned device is indistinguishable from real hardware to KWin and to applications, including games using SDL or Wine. Device name is `clonecast virtual keyboard`, and discovery skips it so clonecast never grabs itself.
*Alternatives.* ydotool: also uinput, but a separate daemon and one more moving part. XTest: X11 only, synthetic events are ignored by many games. wlroots virtual-keyboard protocol: not implemented by KWin.
*Revisit if.* An application whitelists input devices (some anti-cheat). Nothing we can do about it either way.

**3.3 Window control via KWin scripting over DBus, one script per operation.** (2026-09-07)
Wayland gives clients no way to list or focus other clients' windows. KWin exposes a JavaScript scripting API over DBus (`org.kde.KWin /Scripting loadScript` + `run`). Scripts run inside the compositor and can call back over DBus, but cannot be called into. So each operation (list, active, activate) writes a small script to a temp file, loads it, runs it, receives the result on a DBus object clonecast exports (`org.malahmen.clonecast`), and unloads it. This is exactly kdotool's model, and kdotool is the proof it works on Plasma 5 and 6.
Plasma 6 API is used (`workspace.windowList()`, `workspace.activeWindow`, `internalId`). Plasma 5 (`clientList`, `activeClient`) is not supported; Bazzite ships Plasma 6.
*Alternatives.* A persistent script: it could not receive commands, since scripts have no inbound API. `qdbus`/`kdotool` as subprocesses: extra dependency, extra process per key.
*Revisit if.* Latency of a script load per activation proves too high in practice. Then: cache the window list, and investigate whether Plasma 6 exposes a direct activation method on `org.kde.KWin` that avoids the script round trip.

**3.4 Keyboard discovery heuristic: EV_KEY device with both KEY_A and KEY_ENTER.** (2026-09-07)
Keeps laptop and USB keyboards, excludes mice, gamepads, power buttons and multimedia-only devices.
*Revisit if.* Someone's keyboard is split across devices or misdetected. Then add a `--device` flag.

**3.5 Wait 500ms after creating the virtual keyboard.** (2026-09-07)
The compositor needs time to pick up a new input device. Without the wait the first keystrokes vanish. Empirical constant, untested on Bazzite yet.

---

## 4. Broadcast semantics

**4.1 The focused window always receives every key first, via passthrough.** (2026-09-07)
Because of the grab (3.1) this is what keeps the keyboard working. It also defines what "broadcast" means: the window you type in gets the key, the targets additionally get it. If the focused window is itself a target it is skipped in the target loop to avoid a duplicate.

**4.2 Delivery is a focus dance: activate target, wait `SettleDelay`, inject, next; finally re-activate the origin.** (2026-09-07)
There is no per-window injection on Wayland (see 3.3), so this is the only option. Known costs: N focus switches per key, visible flicker, and loose timing for held keys. Every Linux multiboxer hits the same wall.
`SettleDelay` defaults to 30ms and is a flag; too short and the compositor delivers the key to the previous window.
*Revisit if.* Measurements on Bazzite show a better default.

**4.3 Only Down and Up are broadcast; auto-repeat events pass through to the focused window only.** (2026-09-07)
Juggling focus at repeat rate is slow and pointless: each target generates its own repeats from the Down it received.

**4.4 The engine runs in a single goroutine and is the only caller of the injector and window manager.** (2026-09-07)
This is the correctness guarantee that two keys' focus dances never interleave. The UI mutates targets/filter/enabled under a mutex; it never touches the backends directly.

**4.5 Broadcasting starts OFF and the key set starts empty.** (2026-09-07)
Safety: nothing leaves the focused window until the user opts in twice (choose keys, turn on). `--keys` can pre-fill the set; there is deliberately no `--on` flag.

**4.6 A toggle hotkey, default `SCROLLLOCK`, flips broadcasting and is consumed.** (2026-09-07)
Needed because the terminal running clonecast is itself a window: with broadcasting on and `q` in the key set, pressing `q` in the TUI would also go to targets. Scroll Lock is almost never used by applications and often has an LED. Configurable via `--toggle`, and `--toggle ""` disables it.
*Revisit if.* We implement own-window detection (see 7.2), which would make the hotkey a convenience rather than a necessity.

**4.7 Keys are Linux key codes, not characters.** (2026-09-07)
Layout independent and modifier independent: a broadcast "A" is the same physical key on every target. Names are the kernel `KEY_*` names with the prefix stripped, parsed case-insensitively.
Consequence: modifiers are ordinary keys. `Shift+A` reaches targets as `A` unless `LEFTSHIFT` is also in the set. See 7.1.

**4.8 Targets are delivered in window-list order, as ticked in the UI.** (2026-09-07)
Simple and predictable. Windows that vanish are dropped from the selection on the next refresh (every 5s or on `r`).

---

## 5. Project structure

**5.1 Layout.** (2026-09-07)
```
cmd/clonecast/           flags, log setup, backend selection via build tags
internal/keys/           Code, Event, Set (allowlist), name parsing
internal/broadcast/      Source/Injector/WindowManager interfaces + Engine
internal/platform/evdev/ //go:build linux
internal/platform/kwin/  //go:build linux
internal/platform/mock/  portable fake backend
internal/tui/            Bubble Tea model
scripts/setup-bazzite.sh
```
The engine depends only on the three interfaces, so it is fully unit tested with fakes and has no Linux dependency.

**5.2 A mock backend, default on non-Linux, selectable with `--backend mock`.** (2026-09-07)
Development happens on macOS. The mock fakes a Bazzite-like window list, emits a key every 3s, and logs injections and activations. The whole TUI can be exercised without Linux. Verified during bootstrap by driving the binary through a pseudo-terminal: targets ticked, broadcast toggled, focus dance logged, clean quit.

**5.3 Build tags, not runtime checks, separate platform code.** (2026-09-07)
Linux-only packages carry `//go:build linux`; `cmd/clonecast` has `platform_linux.go` and `platform_other.go`. `go vet` is run for both the host and `GOOS=linux` in `make vet` so a change on macOS cannot silently break the Linux build.

**5.4 Flags only, no config file yet.** (2026-09-07)
Keep the bootstrap small. Planned: TOML at `~/.config/clonecast/config.toml` with named key sets and remembered targets by window class.

**5.5 Logs to `~/.cache/clonecast/clonecast.log` via charmbracelet/log.** (2026-09-07)
Stdout belongs to the TUI. Debug level for now.

---

## 6. Distribution and permissions

**6.1 Single static binary in `~/.local/bin`. No Flatpak. No rpm-ostree layering.** (2026-09-07)
Flatpak's sandbox blocks `/dev/input` and `/dev/uinput` unless `--device=all`, which defeats the point of Flatpak. Layering requires a reboot and is heavy for one binary. A static Go binary needs neither.

**6.2 Permissions via the `input` group and a udev rule, installed by `scripts/setup-bazzite.sh`.** (2026-09-07)
- `usermod -aG input` grants read on `/dev/input/event*`.
- `/etc/udev/rules.d/60-clonecast-uinput.rules` sets `/dev/uinput` to `group input, mode 0660`.
- `/etc/modules-load.d/clonecast-uinput.conf` loads `uinput` at boot.
All under `/etc`, which is writable on rpm-ostree systems. Requires a re-login for the group.
*Alternatives.* Running as root: no. A setuid helper or a systemd service holding the devices: more code, same trust level as the group approach.

---

## 7. Open decisions and known gaps

**7.1 Modifier tracking.** Not implemented. Options: (a) require modifiers in the set, document it (current); (b) always broadcast modifier state alongside allowed keys; (c) per-key "with current modifiers" mode. Leaning (b) as a flag.

**7.2 Own-window exclusion.** The terminal running clonecast can be ticked as a target. Detecting our own window means walking from our PID up to the terminal emulator's PID and matching KWin's `pid`. Doable, not done. Until then the toggle hotkey (4.6) is the mitigation.

**7.3 Held keys.** Down and Up are separated by the focus dance, and targets' auto-repeat timing drifts. Acceptable for taps and hotkeys, poor for movement. No good fix exists on Wayland without compositor support.

**7.4 Per-target key sets.** Not planned for v1. One global set.

**7.5 Multiple keyboards.** All are grabbed and merged; the injector clones only the first. Fine unless the first is an odd device.

**7.6 Performance of per-operation KWin scripts.** Unmeasured. See 3.3 for the fallback plan.

**7.7 License.** Not chosen. Owner's call.

**7.8 Nothing Linux-specific has run on hardware yet.** The engine is tested; evdev and kwin compile under `GOOS=linux` and follow known-working patterns (go-evdev, kdotool) but the first Bazzite run is the real test. Step one in README, "First smoke test".

---

## 8. Security and privacy notes

- Grabbing the keyboard means clonecast sees every keystroke, including passwords. Passthrough is not logged. Broadcast events are logged as key names (e.g. `A down -> 2 target(s)`) while broadcasting is on. Turn broadcasting off before typing secrets.
- The DBus name `org.malahmen.clonecast` is claimed exclusively; a second instance fails to start rather than fighting over callbacks.
- KWin scripts are written to a private temp dir (`0600`) and removed after each run.
