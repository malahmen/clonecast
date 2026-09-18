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
3. **Broadcast.** When broadcasting is on and the key is in your key set, clonecast delivers it to every ticked window **except the one you are looking at** — that one already got the key in step 2.

The focused window is therefore the **master**: tick all your clients, and whichever one has focus plays your keys live while the others mirror. \
Switch windows and the master switches with you; the Targets list marks it with `M`. \
By default nothing is broadcast unless the focused window is itself a ticked target, so typing in the terminal that runs clonecast does not drive every client (`--gate-origin=false` lifts that). \
If KWin cannot report which window has focus, that keystroke is not broadcast at all and the log says so — clonecast would otherwise deliver to the window that just received the key through passthrough, doubling it on the client you are playing.

How step 3 reaches an unfocused window is a selectable backend (`--deliver`):

- `agent` (default): a small `clonecast-agent.exe` inside each Wine/Proton prefix receives the key over loopback TCP and `PostMessage`s it to the game window. No focus changes at all. Each agent connects to clonecast by itself and says which prefix it serves, so there is nothing to configure: ticking a window is all it takes.
- `dance` (last resort): the original focus juggling — focus each target, replay the key, restore focus. Fast enough for taps, poor for held movement keys, and rejected for real multiboxing (REFERENCE.md 4.11/4.12). It is the default with `--backend mock`, which has no agents to talk to.
- `xsend`: experimental X11 `XSendEvent`; only reaches clients in a Wine virtual desktop and can stick a key.

See [REFERENCE.md](REFERENCE.md) for every design decision and the reasoning behind it.

## Requirements

- Bazzite (or any KDE Plasma 6 Wayland desktop) in **desktop mode**. Game mode (gamescope) and the GNOME image are not supported.
- Your user in the `input` group and a writable `/dev/uinput`. The setup script handles both.
- `xdotool`, recommended but not required. Without it, focus switches are trusted on KWin's own report alone, which real testing found isn't reliable for actual game windows under real load (see REFERENCE.md 4.10) — keys can silently go nowhere. With it, each switch is independently verified against real focus and retried if needed. The setup script checks for it and prints an install hint if it's missing.

## Install

Download or build `clonecast-linux-amd64` (see Development), then:

```sh
install -Dm755 clonecast-linux-amd64 ~/.local/bin/clonecast
./scripts/setup-bazzite.sh     # adds you to "input", installs a udev rule for /dev/uinput
# log out and back in
```

No Flatpak, no rpm-ostree layering: it is a single static binary and the permissions live in `/etc`, which is writable on Bazzite.

### The in-bottle agent

The `agent` delivery backend needs a small Windows helper running inside each
game's Wine prefix; it is what places keys on an unfocused window without moving
focus. Build it and install it into a prefix:

```sh
make agent                                  # bin/clonecast-agent.exe (GOOS=windows GOARCH=386)
clonecast agent list                        # prefixes of currently running Wine processes
clonecast agent install --prefix ~/.var/app/com.usebottles.bottles/data/bottles/bottles/Malahmen
clonecast agent status  --prefix <path>
clonecast agent uninstall --prefix <path>
```

All four are also in the TUI's **Prefixes** pane (`tab` to it): `r` rescans
running prefixes, `p` points at any prefix by path (including an idle one
`agent list` can't see, since that only finds prefixes with live processes),
`i` installs and `u` uninstalls into whichever is currently selected. Status
for that prefix — installed or not, autostart entry, running or idle — is
fetched automatically as you move, so there's no separate "status" keypress.
`--agent-exe`/`--agent-title`/`--agent-wine` seed that pane's install
defaults the same way `--exe`/`--title`/`--wine` do for the CLI subcommand.

`install` copies the agent to `<prefix>/drive_c/clonecast/` and registers it
under `HKLM\Software\Microsoft\Windows\CurrentVersion\RunServices`, so it
starts on every prefix boot whatever launches the game — Bottles, Lutris, Proton
or umu — and dies with that prefix's wineserver. Nothing on the host ever has to
signal it, which is what used to take the game down with it.

**Agents register themselves.** clonecast listens on one loopback address
(`--listen`, default `127.0.0.1:48800`) and every agent dials in, announcing its
`WINEPREFIX`, its window title, its Windows pid and the window handle it found.
clonecast pairs that announcement with a window in the compositor's list by
reading `WINEPREFIX` out of that window's process (`/proc/<pid>/environ`, using
the host pid KWin gets from the X server), falling back to matching the window
title when the pid or its environment cannot be read. So there is no per-instance
port and no title→port map: a window with a live agent is marked `●` in the
Targets list, and ticking it is the only action.

The agent retries the connection with backoff forever, so it does not matter
whether clonecast or the game starts first. `agent install --port N` is
clonecast's port — the one the agent dials — and the agent also honours
`CLONECAST_PORT` in the prefix's environment (a Steam launch option, a Bottles
env var) and `HKCU\Software\clonecast\Port`, in that order of precedence.

It edits `system.reg` directly when the prefix is idle. If the prefix is already
running, pass the wine command to use instead, for example
`--wine "flatpak run --command=<runner>/bin/wine com.usebottles.bottles"` or
`--wine "PROTON_VERB=run umu-run"`; the command chosen is reported either way.

> Untested against a real prefix. The mechanism is derived from Wine's source
> (see REFERENCE.md 4.16) but has not yet been run on the Bazzite box — verify
> with `research.md` §E step 2 before relying on it.

## Usage

```sh
clonecast                         # agent backend (default): agents register themselves
clonecast --listen 127.0.0.1:48800  # where agents connect (this is the default)
clonecast --deliver dance         # legacy focus dance, no agents needed
clonecast --keys "a b c"          # start with an explicit key set
clonecast --keys all              # broadcast every key
clonecast --toggle PAUSE          # change the on/off hotkey (default SCROLLLOCK)
clonecast --settle 50ms           # more time between focusing a target and injecting (dance)
clonecast --gate-origin=false     # broadcast even when the focused window is not a target
clonecast --agent-exe ./bin/clonecast-agent.exe --agent-wine "flatpak run --command=... com.usebottles.bottles"
                                  # install defaults for the TUI's Prefixes pane
```

Broadcasting always starts **off** and the key set starts **empty**. Flags are only the starting values: the delivery backend, the origin gate, the settle delay, the toggle hotkey and the agent listen address are all editable at runtime in the TUI's Settings pane.

Key names are Linux `KEY_*` names without the prefix, case insensitive: `a`, `F1`, `LEFTSHIFT`, `SPACE`, `KP1`.

Inside the TUI:

| Key                 | Action                                                                        |
| ------------------- | ----------------------------------------------------------------------------- |
| `tab` / `shift+tab` | switch panel (Targets, Keys, Settings, Prefixes, Log)                        |
| `j` / `k`, arrows   | move                                                                          |
| `space` / `enter`   | in Targets: tick the highlighted window; in Settings: change the highlighted setting |
| `a`                 | in Targets: tick/untick every window; in Keys: switch between "all keys" and an explicit set |
| `f`                 | in Targets: focus the highlighted window, making it the master                |
| `e`                 | in Keys: edit the key set; in Settings: type an exact value — settle delay, toggle key, listen address (`enter` applies, `esc` cancels) |
| `i` / `u`           | in Prefixes: install / uninstall the agent in the selected prefix             |
| `p`                 | in Prefixes: point at a prefix by path instead of the running-process list (`enter` applies, `esc` cancels) |
| `b`                 | broadcasting on/off (same as the toggle hotkey)                               |
| `g`                 | origin gate on/off (from any panel)                                           |
| `r`                 | in Targets/Keys/Settings: refresh the window list; in Prefixes: rescan running prefixes (both also auto-refresh) |
| `q`                 | quit                                                                          |

The Settings panel holds the runtime-tunable options: delivery backend, origin gate, settle delay, toggle hotkey and the address agents connect to — and shows how many agents are registered. \
In the Targets list, `M` marks the master, `[x]` a ticked target and `●` a window that has a live in-bottle agent. A ticked window with no agent says `no agent` on its own row: tick it and nothing will reach it until an agent for that prefix registers. \
The Prefixes panel is where that agent gets there in the first place: install, uninstall and status for a Wine prefix, without leaving the TUI (see "The in-bottle agent" above).

The toggle hotkey (`SCROLLLOCK` by default) works from any window and is never delivered anywhere.

Logs go to `~/.cache/clonecast/clonecast.log` (`--log` to change).

## First smoke test on Bazzite

Before trusting it with anything:

1. Run `clonecast --deliver dance --keys a` in Konsole (the dance needs no agents; the agent backend needs one registered per target, and two text editors have none).
2. Open two text editors and tick both as targets with `space` (`a` would tick every window, Konsole included).
3. Press `b`, then click into **one of the editors** and type `a` a few times. That editor is the master: it gets the key directly, the other one gets it from clonecast. Click into the other editor and it swaps roles.
4. Typing `a` in Konsole should broadcast nothing (Konsole is not a target — the log says so once). That is the origin gate; `--gate-origin=false`, or `g` in the TUI, lifts it.
5. Check `~/.cache/clonecast/clonecast.log` for errors. If keys arrive in the wrong window, raise `--settle`.

If a target ignores the injected key, that application reads input in a way uinput cannot satisfy (rare). Note it in an issue.

Then the agent path, with a real game:

1. `make agent`, then either `clonecast agent install --prefix <prefix>` for each game prefix (`clonecast agent list` finds the running ones), or the TUI's Prefixes pane (`tab` to it, `i` on the highlighted prefix).
2. Restart the game so its prefix boots and starts the agent.
3. Run `clonecast`. The header should say `agents: N` and each game's row in Targets should carry `●`; the log says how each agent was paired (by prefix, or by window title as a fallback).
4. Tick every game window — that is the whole of the setup — press `b`, and play on any of them. The one you are looking at is the master; the others mirror it.
5. A ticked row that says `no agent` has none registered for that window: check the agent's log inside the prefix (`<prefix>/drive_c/clonecast/clonecast-agent.log`), which records the address it is dialling.

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
internal/agentwire/      the clonecast <-> agent protocol: hello, keepalive, key frames (tested)
internal/broadcast/      the engine: passthrough + focus-juggle delivery (tested)
internal/platform/agent/ the agent registry: agents connect here and are paired to windows (tested)
internal/prefix/         Wine prefix discovery and `clonecast agent install` (tested)
internal/platform/evdev/ Linux capture (grab) and injection (uinput clone)
internal/platform/kwin/  Linux window listing/activation via KWin scripting over DBus
internal/platform/mock/  fake backend for development
internal/tui/            Bubble Tea front end, incl. the Prefixes pane (tested)
scripts/                 setup-bazzite.sh
```

## Known limitations

- **Modifiers are not tracked.** If `LEFTSHIFT` is not in your key set, targets get `a` when you type `A`. Add the modifier to the set, or use `all`.
- **Held keys** reach targets as a single Down and a later Up, with the focus dance in between. Each target's own auto-repeat kicks in, but timing across targets is loose.
- **The terminal running clonecast** is a window like any other. Do not tick it as a target: with the origin gate on (the default) typing in it broadcasts nothing, which is the point, but ticking it would make it a real target.
- **Wayland native only via KWin.** Any window KWin can list works, including XWayland ones. GNOME, Sway, Hyprland and gamescope are out of scope for now.
- While broadcasting is on, the log records which broadcast keys were pressed. Turn it off before typing passwords.

## License

Not chosen yet. See the open decisions in [REFERENCE.md](REFERENCE.md).
