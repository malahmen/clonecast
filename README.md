# clonecast

Broadcast your keyboard to several windows at once, from a lazygit-style terminal UI.

Pick which running windows receive the keys, and which keys are broadcast: everything, or an explicit set such as `A B C F1`.
Built for [Bazzite](https://bazzite.gg) desktop mode (KDE Plasma 6 on Wayland), written in Go with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

> **Status: pre-alpha.** The core engine is unit tested and the UI runs against a mock backend. \
> The Linux backends (evdev capture, uinput injection, KWin window control) compile but have **not yet been run on real Bazzite hardware**. \
> The first thing to do on a Bazzite box is the smoke test below.

## How it works

1. **Capture.** clonecast grabs your physical keyboard(s) through evdev, so nothing else sees the raw events.
2. **Passthrough.** Every key is replayed through a virtual keyboard to whichever window has focus. Your keyboard keeps working normally.
3. **Broadcast.** When broadcasting is on and the key is in your key set, clonecast asks KWin to focus each selected target in turn, replays the key, and restores focus to where you were.

Step 3 is a focus dance because Wayland offers no way to send input to a window that does not have focus. \
It is fast enough for taps and hotkeys. \
Held movement keys are a known weak spot. \
See [REFERENCE.md](REFERENCE.md) for every design decision and the reasoning behind it.

## Requirements

- Bazzite (or any KDE Plasma 6 Wayland desktop) in **desktop mode**. Game mode (gamescope) and the GNOME image are not supported.
- Your user in the `input` group and a writable `/dev/uinput`. The setup script handles both.

## Install

Download or build `clonecast-linux-amd64` (see Development), then:

```sh
install -Dm755 clonecast-linux-amd64 ~/.local/bin/clonecast
./scripts/setup-bazzite.sh     # adds you to "input", installs a udev rule for /dev/uinput
# log out and back in
```

No Flatpak, no rpm-ostree layering: it is a single static binary and the permissions live in `/etc`, which is writable on Bazzite.

## Usage

```sh
clonecast                         # start with broadcasting OFF and an empty key set
clonecast --keys "a b c"          # start with an explicit key set
clonecast --keys all              # broadcast every key
clonecast --toggle PAUSE          # change the on/off hotkey (default SCROLLLOCK)
clonecast --settle 50ms           # more time between focusing a target and injecting
```

Key names are Linux `KEY_*` names without the prefix, case insensitive: `a`, `F1`, `LEFTSHIFT`, `SPACE`, `KP1`.

Inside the TUI:

| Key                 | Action                                                          |
| ------------------- | --------------------------------------------------------------- |
| `tab` / `shift+tab` | switch panel                                                    |
| `j` / `k`, arrows   | move                                                            |
| `space` / `enter`   | toggle the highlighted window as a target                       |
| `b`                 | broadcasting on/off (same as the toggle hotkey)                 |
| `a`                 | in Keys panel: switch between "all keys" and an explicit set    |
| `e`                 | in Keys panel: edit the key set, `enter` applies, `esc` cancels |
| `r`                 | refresh the window list (also refreshes every 5s)               |
| `q`                 | quit                                                            |

The toggle hotkey (`SCROLLLOCK` by default) works from any window and is never delivered anywhere. Broadcasting always starts **off** and the key set starts **empty**, so nothing is broadcast until you opt in.

Logs go to `~/.cache/clonecast/clonecast.log` (`--log` to change).

## First smoke test on Bazzite

Before trusting it with anything:

1. Run `clonecast --keys a` in Konsole.
2. Open two text editors and tick both as targets.
3. Press `b`, then type `a` a few times in Konsole. Both editors should receive it and focus should return to Konsole.
4. Check `~/.cache/clonecast/clonecast.log` for errors. If keys arrive in the wrong window, raise `--settle`.

If a target ignores the injected key, that application reads input in a way uinput cannot satisfy (rare). Note it in an issue.

## Development

The whole UI can be developed on macOS or any OS with the mock backend, which fakes windows and generates key events every few seconds.

```sh
make run-mock   # TUI with fake windows and keys
make test       # unit tests (engine, key parsing)
make vet        # vet for the host OS and for linux/amd64
make linux      # static bin/clonecast-linux-amd64 for Bazzite
```

Layout:

```
cmd/clonecast/           entrypoint, flags, backend selection (build tags)
internal/keys/           key codes, names, and the allowlist (Set)
internal/broadcast/      the engine: passthrough + focus-juggle delivery (tested)
internal/platform/evdev/ Linux capture (grab) and injection (uinput clone)
internal/platform/kwin/  Linux window listing/activation via KWin scripting over DBus
internal/platform/mock/  fake backend for development
internal/tui/            Bubble Tea front end
scripts/                 setup-bazzite.sh
```

## Known limitations

- **Modifiers are not tracked.** If `LEFTSHIFT` is not in your key set, targets get `a` when you type `A`. Add the modifier to the set, or use `all`.
- **Held keys** reach targets as a single Down and a later Up, with the focus dance in between. Each target's own auto-repeat kicks in, but timing across targets is loose.
- **The terminal running clonecast** is a window like any other. Do not tick it as a target, and use the toggle hotkey when you need to type in it.
- **Wayland native only via KWin.** Any window KWin can list works, including XWayland ones. GNOME, Sway, Hyprland and gamescope are out of scope for now.
- While broadcasting is on, the log records which broadcast keys were pressed. Turn it off before typing passwords.

## License

Not chosen yet. See the open decisions in [REFERENCE.md](REFERENCE.md).
