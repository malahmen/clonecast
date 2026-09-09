# Sending keystrokes into Wine/Proton bottles on Bazzite — research report

Date: 2026-09-09. Scope: how a HotkeyNet-like tool can deliver keystrokes to several Wine/Proton game windows at once, including windows that do **not** have focus, natively on Bazzite (Fedora Atomic, KDE Plasma 6 on Wayland, Steam/Proton, gamescope). Findings come from Wine, Xwayland, KWin and gamescope source, upstream docs, and community reports; every claim carries a link in the Sources section. Where a claim could not be verified in source it is marked *(verify)*.

The focus-dance approach (activate window, inject, restore) is out of scope by decision and only mentioned where a source compares against it.

---

## 1. Summary

1. **Per-window injection into unfocused Wine windows is possible, and the mechanism is X11 `XSendEvent`, not anything on the Wine or Wayland side.** Wine's X11 driver does not check the `send_event` flag on `KeyPress`/`KeyRelease`; it maps the X window id to an HWND and hands the key to wineserver addressed at that window, and wineserver routes it to that window's thread focus even when the window is not foreground. Because wineserver synthesizes Raw Input (`WM_INPUT`), low-level hooks and key state from that same event, games using DirectInput, Raw Input or `GetAsyncKeyState` see it as hardware input. This is strictly more capable than HotkeyNet's own `PostMessage` background mode.
2. **This works under Xwayland on KDE and GNOME without any permission prompt**, because `XSendEvent` is handled entirely inside the X server and never reaches the compositor. Stock Valve Proton still runs games on Xwayland (no Wayland-driver switch exists in Valve's `proton` script; only GE/CachyOS builds have one), so Proton games on Bazzite desktop mode are X clients you can address.
3. **One Wine prefix per game client is the robust configuration.** Raw Input (`WM_INPUT`) is only delivered to the foreground process of a wineserver unless the game registered with `RIDEV_INPUTSINK`; with one prefix per client, a client that has lost X focus has no foreground process in its own wineserver and Wine falls back to the addressed window's thread, so both `WM_KEYDOWN` and `WM_INPUT` arrive. Steam's per-AppID prefix means multiple instances of the same Steam game need extra work (launcher service, umu with distinct `WINEPREFIX`).
4. **Running each client inside its own nested gamescope gives a second per-window path**: gamescope spawns its own Xwayland without libei/portal, so `XTEST` against that instance's `DISPLAY` is handled locally and lands on the game, the only client focused inside that sandbox. gamescope itself injects shortcuts into its Xwayland with `XTestFakeKeyEvent`. This also isolates each client from desktop focus changes.
5. **An agent .exe inside the prefix (HotkeyNet's model) is a complement, not the primary path.** Under Wine, `SendInput` only reaches the foreground window, `PostMessage(WM_KEYDOWN)` bypasses key state and Raw Input, and neither crosses prefixes. Its value is HotkeyNet's `SendWinMF` trick (faking activation for games that ignore input while inactive) and window discovery from inside the bottle. Transport is loopback TCP; Winsock maps to BSD sockets, named pipes are Wine-internal, and `AF_UNIX` is still wine-staging only.
6. **Hotkey capture stays evdev grab plus uinput passthrough.** It is compositor-agnostic, sees every key, and needs no dialogs. Bazzite already ships ydotool, xdotool, input-remapper, evtest and Homebrew; it ships no uinput udev rule, and rules in `/etc/udev/rules.d` need a `udevadm trigger` after install.
7. **Language: Go remains the right call.** `jezek/xgb` (pure Go X protocol including XTEST and `SendEvent`), `holoplot/go-evdev` (grab plus uinput), `godbus/dbus` are all active. The optional in-bottle agent can be the same codebase built with `GOOS=windows CGO_ENABLED=0`; Go binaries run under Wine 9.0-rc2 and later, which every current Proton satisfies.
8. Policy note, stated once: Blizzard has treated input broadcasting to multiple WoW clients as actionable since November 2020 and CCP has banned it in EVE since 2015. Which game is targeted is the user's call; the technique is game-agnostic.

---

## 2. What HotkeyNet does, and what hotkeynet-project is

`MacHu-GWU/hotkeynet-project` is a Python library (`pip install hotkeynet`, MIT, last push 2024-07) that wraps every HotkeyNet block as a Python object and renders a `.hkn` script. Workflow: write Python, run it to emit the script, load the script in HotkeyNet. It exists because HotkeyNet's XML-like DSL has no OOP, no if/else, only text templates, parses long scripts slowly, and the official site is gone; the repo also mirrors the original documentation at hotkeynet.readthedocs.io. Exposed concepts: `Script`, `Hotkey`/`HotkeyUp`, `Label`/`SendLabel`, `Key`/`KeyDown`/`KeyUp`, `SendWin`/`SendWinM`/`SendFocusWin`/`SendPC`, `Toggle`, `MovementHotkey`, `ClickMouse`, `Wait`, `Template`, `Context`.

HotkeyNet's own mechanisms, from the mirrored docs, and what they map to on Linux:

| HotkeyNet mode | Windows mechanism | Reaches background window? | Linux/Wine equivalent found |
|---|---|---|---|
| `SendWin` | `SendInput`, brings window to foreground first | No (foreground only, alt-tabs) | uinput / libei to focused window; the deprecated focus dance |
| `SendFocusWin` | `SendInput` to whatever has focus | No | uinput passthrough (clonecast already does this) |
| `SendWinM` | `PostMessage(WM_KEYDOWN/UP)` | Yes, "works with some applications but not all"; trouble with Alt/Ctrl combos | In-prefix agent `PostMessage`; **superseded by XSendEvent**, which also updates key state and Raw Input |
| `SendWinMF` | `PostMessage` plus faked foreground/activation state | Yes, for games that ignore input while inactive | In-prefix agent only (needs Win32 messages) |
| `SendWinS`/`SendWinSF` | synchronous `SendMessage` | Yes, very slow | Same as above |
| `SendPC` | TCP to another HotkeyNet instance | n/a | Daemon-to-daemon TCP; also daemon-to-agent |
| `WH_KEYBOARD_LL` hotkeys | low-level hook | n/a | evdev grab |

HotkeyNet 1.3 (2009) is unmaintained and Windows-only. Under Wine it has no AppDB entry; reports say it runs but key broadcast fails, and AutoHotkey under Wine has the same limitation. A 2021 Dual-Boxing setup made it work only by running one HotkeyNet client inside **each** Wine prefix with the server in a Windows VM, which is the prefix-isolation constraint in section 4.

---

## 3. The environment on Bazzite

- **Compositor.** KDE Plasma 6 on Wayland (GNOME image exists). Desktop mode runs one rootless Xwayland for all X clients; its display is whatever `$DISPLAY` says in the session (commonly `:1` on Plasma Wayland, `:0` on some setups). Verify on the box.
- **Steam and Proton.** Bazzite desktop ships native (RPM) Steam and Lutris, not Flatpak. Stock Valve Proton 10 and 11 run games through winex11.drv on Xwayland. The `proton` script in Valve's `proton_10.0`, `proton_11.0` and `experimental_11.0` branches has no `PROTON_ENABLE_WAYLAND`; that switch exists only in GE-Proton, CachyOS Proton and Proton-EM. Wine 10.0 enabled winewayland.drv by default but X11 still wins when `DISPLAY` is set; Wine 11.x still calls it experimental.
- **Prefixes.** Each `WINEPREFIX` has its own wineserver; windows in different prefixes cannot see each other (`FindWindow`, `PostMessage` do not cross). Steam uses one prefix per AppID under `steamapps/compatdata/<appid>/pfx`. Lutris and Bottles use one prefix per game or bottle; multiple instances of a non-Steam game normally share a prefix unless you clone it.
- **Steam Linux Runtime.** Proton games run inside a pressure-vessel container. `$HOME`, `/tmp` and `XDG_RUNTIME_DIR` are shared with the host; the X socket is shared; the network namespace is not isolated (games reach the internet). Anything spawned from a launch-options wrapper runs outside the container unless you join it (section 7).
- **Gaming Mode.** gamescope-session shows one game window at a time (`STEAM_GAME` property, later windows overlay the main one). Multiboxing with visible clients happens in Desktop Mode, optionally with one nested gamescope per client.
- **Immutable OS.** `/usr` is read-only, `/etc` and `/var` are writable. Homebrew is preinstalled. Flatpak sandboxes block `/dev/input` and `/dev/uinput`. rpm-ostree layering is documented as a last resort.

---

## 4. How Wine consumes keyboard input (from source)

These are the facts the whole design rests on. Paths are in wine-mirror master; Valve's `proton_10.0` tree matches on every point checked.

**X11 driver, event dispatch.** `dlls/winex11.drv/event.c` dispatches `KeyPress`/`KeyRelease` straight to `X11DRV_KeyEvent`. `filter_event()` only tests the event type against the queue mask. The only `send_event` checks in winex11.drv are for `ConfigureNotify`/`GravityNotify` geometry. So synthetic key events (XSendEvent) are processed like real ones. The Arch forum claim that "wine filters xsendevent events since ever" is not supported by the source.

**X11 driver, key handling.** `X11DRV_KeyEvent` in `keyboard.c` runs `XLookupString` (or `XmbLookupString` if XIM is on; Proton disables XIM by default), computes the scancode with `keyc2scan(event->keycode, event->state)`, and calls `X11DRV_send_keyboard_input()` which ends in `NtUserSendHardwareInput(hwnd, 0, &input, 0)`. No focus check. The `hwnd` comes from `XFindContext(event->xany.window, winContext)`; Wine registers both the `whole_window` and the `client_window` of each top-level HWND in that context (`window.c`), so either X id resolves. An X id that is not a registered Wine window yields `hwnd = 0`.

**Server routing.** In `server/queue.c`, `queue_hardware_message`: if `msg->win` is set, the target input queue is that window's thread queue; otherwise `desktop->foreground_input`. For key messages the destination is `input->focus`, falling back to `input->active` (as `WM_SYSKEY*`). Consequence: a key addressed to a Wine window's X id reaches that window's thread focus window whether or not it is foreground or X-focused.

**Raw Input.** `queue_keyboard_message` builds a `RIM_TYPEKEYBOARD` `WM_INPUT` from every keyboard hardware input and `dispatch_rawinput_message` fans it out to processes that called `RegisterRawInputDevices`. Only the foreground process receives `WM_INPUT` unless the device was registered with `RIDEV_INPUTSINK`. `get_foreground_thread` falls back to the receiving window's thread when the desktop has no foreground input. Wine's X11 driver uses XInput2 only for raw mouse motion (Valve adds raw buttons); there is no `XI_RawKeyPress` path, so keyboard Raw Input is always synthesized from core `KeyPress`. wine-staging's `user32-rawinput-keyboard` patchset is `Disabled: true`.

**DirectInput.** `dlls/dinput` feeds keyboard devices from a `WH_KEYBOARD_LL` hook, or from Raw Input when `use_raw_input` is set (`RIDEV_INPUTSINK` for `DISCL_BACKGROUND`). Low-level hooks are dispatched by wineserver for all hardware keyboard input on the desktop, and `LLKHF_INJECTED` is set only for `SendInput`-origin events, so X-driver input, synthetic or not, looks like hardware to hooks. `DISCL_FOREGROUND` devices are unacquired via a CBT hook when their window loses foreground; that is a per-game behavior identical to Windows.

**Win32 injection APIs inside a prefix.** `SendInput`/`keybd_event` call `NtUserSendInput` → `server_send_hardware_message(0, SEND_HWMSG_INJECTED, ...)` with hwnd 0, so they go to `desktop->foreground_input` only. `PostMessage(WM_KEYDOWN)` goes to the message queue but bypasses key state, Raw Input and hooks. `SetForegroundWindow` is refused with `STATUS_ACCESS_DENIED` when the caller is not the foreground process and its input `user_time` is older than the foreground's; actual X focus is additionally subject to KWin focus-stealing prevention.

**Wayland driver.** `winewayland.drv/wayland_keyboard.c` takes `wl_keyboard` events for the focused surface and calls `NtUserSendHardwareInput` from the focused hwnd. There is no per-window injection path; only compositor-level input (libei/portal, uinput) reaches it.

---

## 5. Delivery techniques evaluated

### 5.1 Comparison

| Technique | Reaches an unfocused Wine window? | Raw Input / DirectInput games? | Under Xwayland on KDE/GNOME | Permission | Cross-prefix | Maturity |
|---|---|---|---|---|---|---|
| **A. `XSendEvent` KeyPress/Release to the Wine X window** | **Yes** (routed to that window's thread focus) | **Yes** (WM_INPUT, hooks, key state synthesized from the same event); needs one prefix per client or `RIDEV_INPUTSINK` | Yes, pure X-server op, no prompt | none | Yes (any X window on the display) | Core X11; xdotool `--window` uses it |
| **B. XTEST or XSendEvent on a nested gamescope's `DISPLAY`** | Yes relative to the desktop (game is focused inside its sandbox) | Yes | Yes (nested Xwayland has no EI socket, handles XTEST locally) | none | Yes (one display per client) | gamescope injects its own shortcuts this way; few public reports *(verify)* |
| **C. In-prefix agent: `PostMessage(WM_KEYDOWN)`** | Yes (message queue) | No (bypasses key state/rawinput/hooks) | n/a | none | No, one agent per prefix | HotkeyNet `SendWinM`; known modifier trouble |
| C'. In-prefix agent: `SendInput` | No, foreground only | Yes, foreground client only | n/a | none | No | Same as Windows |
| C''. In-prefix agent: fake activation + PostMessage (`SendWinMF`) | Yes, for games that ignore input while inactive | No | n/a | none | No | HotkeyNet-specific trick |
| D. uinput virtual keyboard (ydotool, dotool, own device) | No, focused window only | Yes (indistinguishable from hardware) | Yes, everywhere incl. gamescope-session | `/dev/uinput` udev rule + `input` group | n/a | Mature |
| D'. libei via RemoteDesktop portal `ConnectToEIS` | No, focused only | Yes | KDE 6.1+, GNOME 45+; dialog once, `persist_mode=2` | portal consent | n/a | Maturing; KDE private `org.kde.KWin.EIS.RemoteDesktop` avoids the dialog |
| D''. XTEST on the desktop Xwayland | No, routed by KWin/Mutter to the Wayland-focused surface | Yes | Works after the KWin "Legacy X11 App Support" prompt (Plasma 6.4+ has "allow without asking") or GNOME portal prompt | prompt | n/a | Fine for the leader window only |
| E. wtype (`zwp_virtual_keyboard_v1`) | No | — | **Not implemented by KWin or Mutter** | — | — | wlroots only |
| E'. KDE `org_kde_kwin_fake_input` | No | — | Internal-only protocol, clients told not to use it | — | — | Legacy |
| E''. winewayland.drv targets | No X id exists; only Wayland focus input | — | Not reachable by any X tool | — | — | Only GE/CachyOS Proton enable it |

### 5.2 Technique A: XSendEvent to the Wine X window (primary)

**Why it works.** Section 4: winex11.drv ignores `send_event`, resolves the X window to an HWND, and wineserver delivers to that window's thread. Key state, `WM_INPUT` and low-level hooks are all derived server-side from that one event, so the game cannot distinguish it from a physical key on the X layer. `XSendEvent` is a core X protocol request executed inside the X server; on Xwayland the compositor is not involved and no libei/portal prompt appears.

**Preconditions and details.**
- **Target the right X id.** Use the game's top-level X window (Wine registers both whole and client windows). Community reports that worked with WoW targeted the client's "Default IME" X window, which belongs to the game thread and therefore routes the same way. If the id is not a Wine window the event resolves to `hwnd = 0` and goes to the prefix's foreground input, which is a silent misroute.
- **One prefix per client for Raw Input games.** With several clients in one wineserver only the foreground client receives `WM_INPUT`. With one prefix per client, a client that lost X focus has `desktop->foreground_input` cleared (X11 `FocusOut` moves Wine's foreground to the desktop), and `get_foreground_thread` falls back to the addressed window, so both `WM_KEYDOWN` and `WM_INPUT` arrive. Games that only read `WM_KEYDOWN`/`WM_CHAR` work in a shared prefix too.
- **Keycodes.** Xwayland uses evdev keycodes plus 8, so a Linux `KEY_*` code from the evdev grab maps directly to the X keycode without a keymap lookup. Wine's `keyc2scan` is built from the X server keymap, so this is layout-safe.
- **Modifiers.** Wine reads `event->state` for lookup and tracks modifier keys as ordinary key events. Send modifier press/release events yourself and set the corresponding `state` mask bits on the key events between them. Do not rely on xdotool's `--clearmodifiers`, which manipulates real modifier state via XTEST.
- **Event fields.** `type`, `window` (target), `root`, `subwindow=None`, `time=CurrentTime`, `x/y/x_root/y_root` (0 is fine), `state`, `keycode`, `same_screen=True`. Send with `propagate=False` and `event_mask=KeyPressMask|KeyReleaseMask`; Wine selects those masks on its windows.
- **Auto-repeat.** Only send Down and Up; the game or Wine does not auto-repeat synthetic keys, so held keys need explicit repeat events from the sender if the game relies on repeats (most games poll key state and do not).

**Reported caveats to verify empirically.**
- Dual-Boxing (2019) reports xdotool reaching *background* WoW clients only when each client ran in its own prefix inside a Wine virtual desktop; `multiwow` hard-requires virtual desktop mode. The code reading does not explain why fullscreen non-virtual-desktop windows would differ; possibilities are the X id chosen, fullscreen `whole_window` recreation on mode switch, or foreground-input interaction. Test both configurations.
- WineHQ forum snippets mention xdotool reaching Wine games "only if unfocused" and fullscreen trouble. Consistent with the routing above (an X-focused window also receives real input) but check for duplicate delivery when the target is also the focused window; clonecast already skips the focused target.
- Games that stop processing input on `WM_ACTIVATE(inactive)` or that use `DISCL_FOREGROUND` DirectInput ignore background keys under Wine exactly as on Windows. That is where technique C'' (fake activation) or technique B (each client believes it is focused) applies.
- xdotool 3.20210804.2 and later may refuse to run when it detects Xwayland; implement the X calls directly rather than shelling out to xdotool. The check is about XTEST on the desktop display, not `XSendEvent`, but it affects the binary regardless.
- Anti-cheat that whitelists input devices cannot be addressed by any of these techniques; note and move on.

### 5.3 Technique B: one nested gamescope per client, XTEST per DISPLAY

gamescope spawns its Xwayland through wlroots with `-rootless -core -terminate -listenfd ...`; neither wlroots nor gamescope passes `-enable-ei-portal`, and no `LIBEI_SOCKET` is set. Xwayland's `xwayland-xtest.c` then logs "EI failed, using XTEST as fallback" and processes XTEST locally, delivering to the X client focused inside that server, which is the game. gamescope relies on this itself: `wlserver.cpp` calls `XTestFakeKeyEvent()` on its own Xwayland to inject Ctrl+key shortcuts. Technique A (`XSendEvent`) works there as well.

**Benefits.** Per-client display makes targeting trivial (`DISPLAY=:N`), each client is always "X-focused" inside its sandbox, which sidesteps games that ignore inactive input *(verify that gamescope keeps X focus on the game when the host gamescope window loses Wayland focus)*, and each client has its own X keyboard state so modifier handling is isolated.

**Costs and known issues.** Must discover each instance's display (parse gamescope's stderr, or launch with a known `--xwayland-count`/let the daemon start gamescope and read its display). Nested gamescope on Wayland desktops has reports of Super+key shortcuts needing `--backend sdl`, Shift/Caps combinations not registering, keyboard layout not imported from the parent, and `--force-grab-cursor` losing the grab after interacting with other windows. Extra GPU compositing per client. SteamTinkerLaunch notes you cannot switch the active window inside a nested gamescope, which is fine for one game per instance. Launch shape in Steam: `gamescope -W 1280 -H 720 -- %command%`.

### 5.4 Technique C: in-prefix Windows agent

An agent .exe running in each bottle, listening on loopback TCP, offers three things the X layer cannot:
1. `SendWinMF`-style fake activation: post `WM_ACTIVATE`/`WM_SETFOCUS`/`WM_NCACTIVATE` around `WM_KEYDOWN` for games that drop input while inactive.
2. Window discovery from inside the bottle (`EnumWindows`, class names, PIDs) and mapping HWND to X id via Wine's `X11DRV` properties if needed.
3. A path for winewayland.drv setups, where no X id exists (PostMessage only, with its limitations).

Limits: `SendInput` is foreground-only in Wine as in Windows; `PostMessage` bypasses key state and Raw Input; one agent per prefix; `SetForegroundWindow` is gated by wineserver's `user_time` rule and by KWin.

**Transport.** Wine's `ws2_32` is implemented on BSD sockets, so `connect()` to `127.0.0.1:<port>` reaches a Linux listener, also from inside pressure-vessel (no network isolation). Named pipes are wineserver-internal and invisible to Linux (winestreamproxy and outflow exist to bridge them). `AF_UNIX` in ws2_32 is a wine-staging patchset, still experimental as of Wine Staging 11.16; do not depend on it. `Z:` maps `/`, so a FIFO under `$HOME` is reachable as a file, but FIFO semantics through Wine's file layer are awkward and a Unix socket file cannot be opened at all. Use TCP with a small length-prefixed protocol.

**Building the agent.** Go with `GOOS=windows CGO_ENABLED=0` works under Wine 9.0-rc2 and later (Go 1.21.5+ needs `bcryptprimitives.dll!ProcessPrng`, which Wine added in 9.0-rc2; all current Proton, GE-Proton and umu Proton qualify). `golang.org/x/sys/windows` exports `EnumWindows`, `GetForegroundWindow`, `GetClassNameW`, `GetWindowThreadProcessId`, `IsWindowVisible`, `GetGUIThreadInfo`, `MapVirtualKey`-adjacent helpers, but **not** `PostMessageW`, `SendInput`, `FindWindowW`, `SetForegroundWindow`, `MapVirtualKeyW`; load those with `windows.NewLazySystemDLL("user32.dll").NewProc(...)`. Rust (`x86_64-pc-windows-gnu`) has the identical `ProcessPrng` requirement and buys nothing over Go here. A mingw-w64 C agent is the fallback for a ~50 KB binary or ancient Wine.

How to launch the agent inside the game's prefix is in section 7.

### 5.5 Technique D: compositor-level injection (focused window only)

Used for passthrough to the leader window, not for broadcast.
- **uinput** (own virtual device, or ydotool/dotool): works on KDE, GNOME and inside gamescope-session; needs `/dev/uinput` access. Cloning the physical keyboard's capabilities makes the device indistinguishable from hardware. ydotool needs a persistent daemon because compositors take time to pick up a new device; clonecast's 500 ms wait after device creation is the same workaround.
- **libei via RemoteDesktop portal.** No root; KDE Plasma 6.1+ and GNOME 45+; `SelectDevices(persist_mode=2)` plus `restore_token` persists the grant, though GNOME users reported no permanent-allow in practice as of late 2025 and KDE had token-persistence bugs (480235). KDE exposes a private `org.kde.KWin.EIS.RemoteDesktop.connectToEIS` D-Bus method that returns an EI fd with no dialog. Bindings: C libei/liboeffis, Rust `reis` 0.7 + `ashpd`, Python `python-libei` 0.5 (ctypes), Go `bnema/libei-go-bindings` (cgo, pre-1.0) or robotgo's pure-Go libei backend.
- **XTEST on the desktop Xwayland.** Since Xwayland 23.2, rootless Xwayland forwards XTEST to the compositor via libei (portal on GNOME, KWin's own EIS socket on KDE). KWin shows a "Legacy X11 App Support" prompt; Plasma 6.4+ has "Control of pointer and keyboard: Allow without asking for permission". Once allowed, keys go to whatever KWin has focused, never to a specific X window.

### 5.6 Rejected or not applicable

- **wtype** and the `zwp_virtual_keyboard_v1` protocol: not implemented by KWin or Mutter.
- **KDE fake-input protocol**: marked an implementation detail that regular clients must not use; KWin's EIS backend has no public socket, clients are expected to use the portal.
- **GlobalShortcuts / InputCapture portals for broadcast keys**: GlobalShortcuts only fires pre-declared, user-approved combos (fine for a few control hotkeys like toggle); InputCapture only activates after a pointer-barrier crossing and KDE has not implemented it.
- **winewayland.drv**: no per-window path at all. Keep clients on Xwayland; do not set `PROTON_ENABLE_WAYLAND`.
- **HotkeyNet or AutoHotkey under Wine**: HotkeyNet runs but broadcast fails; AHK `Send` is unreliable under Wine; both are confined to their own prefix. One HotkeyNet client per prefix plus a Windows VM server has worked, which is the architecture of technique C with worse tooling.
- **Focus dance**: deprecated by project decision.

---

## 6. Capture side: reading the physical keyboard while a game is focused

| Method | KDE | GNOME | gamescope-session | Permission | Sees every key | Latency |
|---|---|---|---|---|---|---|
| **evdev grab (`EVIOCGRAB`) + uinput re-emit** | Yes | Yes | Yes | `input` group + `/dev/uinput` | Yes, and can swallow or rewrite | ~1 ms |
| evdev read without grab | Yes | Yes | Yes | `input` group | Yes, but the focused window also gets the key | sub-ms |
| GlobalShortcuts portal | Yes | GNOME 48+ | No | dialog | Only pre-declared combos | D-Bus hop |
| kglobalaccel D-Bus | Yes | No | No | none | Only registered combos | D-Bus hop |
| InputCapture portal | No (KDE open issue) | Yes | No | dialog | Only after barrier crossing | — |

The grab-and-re-emit model is what keyd, kanata, xremap, evremap and input-remapper (preinstalled on Bazzite) use. It is the right capture layer, and clonecast already implements it. The portal or kglobalaccel is only worth adding for a handful of fixed control hotkeys (toggle, switch leader) if you want them to show up in System Settings.

Keyboard discovery heuristic (EV_KEY with `KEY_A` and `KEY_ENTER`), cloning the first keyboard into the virtual device, and the 500 ms settle after creation are all consistent with what ydotool and kanata document.

---

## 7. Running a helper inside a specific prefix (for technique C and for multi-instance setups)

**What `%command%` expands to.** For a Proton title: `reaper SteamLaunch AppId=N -- steam-launch-wrapper -- SteamLinuxRuntime_sniper/_v2-entry-point --verb=waitforexitandrun -- "<Proton>/proton" waitforexitandrun game.exe`. A wrapper script receives this as `"$@"`; anything it spawns itself runs **outside** the pressure-vessel container.

**The `proton` script.** Requires `STEAM_COMPAT_DATA_PATH`; verbs are `run`, `waitforexitandrun` (does `wineserver -w` first, so it waits for the running game to exit), `runinprefix`, `destroyprefix`, `getcompatpath`, `getnativepath`. To add a process to a prefix where the game is already running use `run` or `runinprefix`, never `waitforexitandrun`. Legacy recipe: `STEAM_COMPAT_CLIENT_INSTALL_PATH=~/.steam/root STEAM_COMPAT_DATA_PATH=.../compatdata/<appid> "<Proton>/proton" run agent.exe`; reports say this became unreliable after Proton 6 when Steam holds the prefix.

**Cleanest for Steam: join the game's container.** Put `STEAM_COMPAT_LAUNCHER_SERVICE=proton %command%` in the game's launch options. Steam then exposes a D-Bus launcher for that container and `steam-runtime-launch-client --bus-name=com.steampowered.App<appid> --directory='' -- wine Z:\path\agent.exe` runs inside the same container and wineserver, so `FindWindow`/`PostMessage` see the game. This is also the documented route to run **multiple instances of one Steam game** (apple1417's write-up). `protonhax` (bash, 2024) wraps the same idea: `protonhax init %command%` then `protonhax run <appid> C:\path\agent.exe`.

**umu-launcher** is preinstalled on Bazzite. Env: `WINEPREFIX` (point at `.../compatdata/<appid>/pfx`), `GAMEID`, `PROTONPATH` (absolute path for Valve's official Proton dirs; umu only auto-discovers `compatibilitytools.d`), `PROTON_VERB=run` or `runinprefix` (default is `waitforexitandrun`), `--config file.toml`. All Proton settings must match the running game's; umu starts its own SLR container, so sharing wineserver with Steam's container depends on `/tmp` visibility *(verify)*.

**protontricks** (`protontricks-launch --appid <APPID> agent.exe`, `protontricks -c <cmd> <APPID>`) starts its own bwrap container and wineserver keepalive; fine for installers, weaker for a co-resident agent.

**Lutris**: no CLI for "Run EXE inside Wine prefix"; use `lutris -b` to dump the game's exact env and wine binary, then run the runner's `wine` with that `WINEPREFIX`. **Bottles**: `bottles-cli run -b <Bottle> -e /path/agent.exe` (prefix with `flatpak run --command=bottles-cli com.usebottles.bottles` when Flatpak). **Heroic**: per-game "Run EXE on Prefix" GUI tool and a `runInPrefix` config array; Flatpak Heroic cannot see exes outside its sandbox.

---

## 8. Language and library survey

Checked September 2026.

**Go (recommended, matches the existing codebase).**
- `holoplot/go-evdev`: pure Go evdev with `EVIOCGRAB` and uinput including `CloneDevice`; active (push 2026-09).
- `jezek/xgb` and `jezek/xgbutil`: pure Go X protocol with XTEST, XInput, core `SendEvent`, EWMH helpers (`_NET_CLIENT_LIST`, `_NET_WM_PID`, `WM_CLASS`); active fork of BurntSushi (push 2026-04).
- `godbus/dbus` v5: pure Go D-Bus; active.
- `bendahl/uinput`: archived 2024-03, feature complete; superseded here by go-evdev's uinput.
- `gvalkov/golang-evdev`: archived.
- `bnema/libei-go-bindings`: cgo, 0 stars, not production. `go-vgo/robotgo` ships pure-Go X11, wlroots and libei backends, build-tag selected, experimental; the most practical libei route in Go if ever needed.
- Windows side: `golang.org/x/sys/windows` plus `NewLazySystemDLL("user32.dll")` for the missing calls; `icio/go-wine-test` is a minimal cross-compile-and-run-under-Wine demo.

**Rust (equal capability).** `evdev` 0.13 (has uinput), `evdevil` 0.5, `x11rb` 0.14, `reis` 0.7 (pure Rust libei/libeis), `ashpd` 0.13 (portals), `windows-sys` for the agent; `enigo` already wires ashpd+reis. Fresher libei story than Go, otherwise no advantage for this project.

**Python (prototyping).** `python-evdev` 2.0 (2026-08, includes UInput) is active; `python-xlib` last release 2022; `pydbus` effectively dead (use `dbus-next`/`dasbus`); `python-libei` 0.5 (2026-09, beta). Packaging on an immutable OS is worse than a static binary.

**C.** libevdev, libxdo (xdotool 4.2026, X11 only), libei 1.6 + liboeffis, mingw-w64 for the agent; smallest binaries, most boilerplate.

---

## 9. Distribution and permissions on Bazzite

- **Install order Bazzite documents**: ujust/Bazzite Portal, Flatpak (Bazaar), Homebrew for CLI, Distrobox, AppImage, rpm-ostree layering last. Homebrew is preinstalled (`/home/linuxbrew/.linuxbrew`) but cannot install anything needing root.
- **Best fit**: a single static binary in `~/.local/bin` (optionally a Homebrew tap for updates) plus a systemd **user** unit in `~/.config/systemd/user/`. Flatpak is a poor fit (needs `/dev/input`, `/dev/uinput`, and to spawn Steam-runtime tooling).
- **udev rule**: `/usr/lib/udev/rules.d` is read-only; put it in `/etc/udev/rules.d/`. Two accepted forms: Valve's `KERNEL=="uinput", SUBSYSTEM=="misc", TAG+="uaccess", OPTIONS+="static_node=uinput"` (steam-devices), or `MODE="0660", GROUP="input"` plus the user in `input` (needed anyway for `/dev/input/event*`). Bazzite ships no uinput rule of its own. Bazzite bug 2516: rules in `/etc/udev/rules.d` were not applied at boot until `udevadm control --reload-rules && udevadm trigger`; the setup script should run that and the docs should say to verify after the first reboot. `OPTIONS+="static_node=uinput"` avoids depending on module load order; a `/etc/modules-load.d` entry is still fine.
- **Groups**: `usermod -aG input` can fail because system groups live in `/usr/lib/group`; the documented procedure is `grep '^input:' /usr/lib/group | sudo tee -a /etc/group` then `sudo usermod -aG input $USER`. Bazzite Portal has a one-click "Add input to your user groups". Re-login required.
- **Already on Bazzite**: ydotool, xdotool, input-remapper (service enabled on desktop), libinput-utils, evtest, inputplumber, hid-replay, umu-launcher, protontricks (check), Lutris, native Steam.

---

## 10. Proposed architecture for clonecast

Capture stays as is. Delivery is rebuilt around X11 per-window injection, with nested gamescope as the isolation option and an optional in-bottle agent for the games that need Win32-level tricks.

```
physical keyboards ──evdev grab──▶ engine ──uinput clone──▶ focused window (passthrough, unchanged)
                                     │
                                     ├──XSendEvent──▶ Wine X window A   (desktop Xwayland, $DISPLAY)
                                     ├──XSendEvent──▶ Wine X window B
                                     ├──XTEST/XSendEvent──▶ DISPLAY=:N  (client C in its own gamescope)
                                     └──TCP 127.0.0.1──▶ agent.exe in prefix D ──PostMessage/fake-activate──▶ game D
```

**New package: `internal/platform/x11/`** (build tag linux), on `jezek/xgb`:
- `Connect(display string)`; one connection per display (desktop Xwayland plus each nested gamescope).
- `ListWindows()` via `_NET_CLIENT_LIST` on the root window, reading `WM_CLASS`, `_NET_WM_NAME`, `_NET_WM_PID`, and Wine's `WM_CLASS` conventions (`steam_app_<id>`, `<exe>.exe`). Filter to windows whose PID runs under wine (check `/proc/<pid>/exe` or `WM_CLASS`), so non-Wine windows are not offered as targets, or offer everything and let the user pick.
- `SendKey(win, code keys.Code, down bool, mods Modifiers)`: build a `KeyPressEvent`/`KeyReleaseEvent` with `Detail = uint8(code+8)`, `State` from tracked modifiers, `Root`, `SameScreen=true`, and call `xproto.SendEvent(conn, false, win, KeyPressMask|KeyReleaseMask, event)`. Flush after each batch.
- Modifier tracking in the engine: maintain the Linux modifier state from the grabbed stream and, per target, emit modifier Down/Up around the broadcast key when the key set includes modifiers, plus the matching `state` bits.

**Window identity across layers.** The TUI currently lists KWin windows (UUID, caption, resourceClass, pid). X windows have a different id space. Options: (a) list from X only (`_NET_CLIENT_LIST`), which covers all Proton/Wine targets and drops the D-Bus dependency entirely; (b) keep KWin for listing and join on PID. Option (a) is simpler and sufficient because every target is an X client. KWin remains useful only for the deprecated activation path, which can be deleted.

**Gamescope option.** A `--gamescope` launch helper (or documentation) that starts `gamescope -W … -H … -- <game command>` per client, captures the nested display number, and registers that display with the engine. Targets on nested displays are addressed via XTEST (focused inside the sandbox) or XSendEvent (same code path as the desktop).

**Optional agent (`cmd/clonecast-agent`, `GOOS=windows`).** Listens on a loopback port, one instance per prefix, launched via the launcher service, umu or `bottles-cli`. Protocol: length-prefixed frames `{key, down, mods, mode}` with modes `post` (PostMessage), `post-activate` (fake activation around the message), `sendinput` (foreground only). The daemon connects when the user enables "agent mode" for a target. This is phase 2, only after technique A is measured against real games.

**REFERENCE.md entries affected**: 1.3 (XWayland windows are targets via KWin) becomes "targets are X windows on the Xwayland display(s)"; 3.3 (KWin scripting per operation) and 4.2 (focus dance) are withdrawn; 4.3 (auto-repeat) holds; 7.1 (modifier tracking) becomes required, not optional; 7.3 (held keys) is largely solved since Down and Up no longer straddle a focus change; 2.4 (godbus) may become unnecessary.

---

## 11. Verification plan, in order

Each step has a pass criterion. Do them on the Bazzite box in desktop mode.

1. **Confirm the X layer.** In Konsole: `echo $DISPLAY`, then with a Proton game running `xprop -root _NET_CLIENT_LIST` and `xprop -id <id> WM_CLASS _NET_WM_PID`. Pass: the game window appears with a `steam_app_<id>` or `<exe>.exe` class.
2. **XSendEvent to a background Wine window.** Small Go or Python test using `SendEvent` (not xdotool, which may refuse Xwayland): send `a` Down/Up to the game's window id while Konsole keeps focus, in a game text field (chat box). Pass: the character appears without the game being focused and without any KWin prompt. Repeat with the game fullscreen, and with the client in a Wine virtual desktop, to settle the 2019 Dual-Boxing report.
3. **Raw Input / DirectInput path.** Same test bound to a game action rather than a text field (a game that uses Raw Input or DirectInput; a `GetAsyncKeyState`-polling game is a good third case). Pass in one prefix per client. Then put two clients in one prefix and observe whether the non-foreground one still reacts; expected: message-reading games yes, Raw Input games no.
4. **Modifiers.** Send LEFTSHIFT Down, `a` Down/Up with `state=ShiftMask`, LEFTSHIFT Up. Pass: uppercase `A`. Then Ctrl/Alt combos, which HotkeyNet's background modes struggled with.
5. **Duplicate delivery.** Target the focused window as well and confirm the engine's skip prevents doubles; also confirm no double when passthrough uinput and XSendEvent both arrive at a target (they should not, since passthrough only reaches the focused window).
6. **Nested gamescope.** Launch a client with `gamescope -W 1280 -H 720 -- %command%`, find its display (`ls /tmp/.X11-unix`, or gamescope stderr), run the XTEST and XSendEvent tests against it while another window has desktop focus. Pass: the game reacts. Then check whether the game still reacts after the gamescope window itself loses focus for a while (does gamescope tell the game it is inactive?).
7. **Inactive-window behavior per game.** For each target game, note whether it processes background keys at all. Games that do not are candidates for the agent's fake-activation mode or for the gamescope sandbox.
8. **Agent transport.** Build a hello-world `GOOS=windows` binary that dials `127.0.0.1:port`, run it inside the prefix via `STEAM_COMPAT_LAUNCHER_SERVICE=proton` + `steam-runtime-launch-client`, and via `PROTON_VERB=run umu-run`. Pass: connection established from inside pressure-vessel; `EnumWindows` from the agent lists the game window.
9. **Permissions on first boot.** After running the setup script and rebooting, check `ls -l /dev/uinput` and `id` show the rule and group took effect without a manual `udevadm trigger`.

---

## 12. Risks and open questions

- **Game-specific inactive-input handling** is the main unknown; it is the same wall HotkeyNet hit on Windows, and the per-game answer decides whether A alone suffices or B/C are needed.
- **The 2019 reports tying success to Wine virtual desktop mode** are unexplained by the source reading. If step 2 reproduces them, inspect which X window Wine registers for the fullscreen client and whether `hwnd` resolves.
- **Multiple instances of one Steam game** need distinct prefixes for the Raw Input path; the launcher-service route or umu with distinct `WINEPREFIX` is more setup than Lutris/Bottles clones.
- **Proton moving to winewayland.drv by default** would remove the X id. Valve has not done this as of Proton 11; GE/CachyOS make it opt-in. Watch Proton release notes.
- **KWin "Legacy X11 App Support" prompt** does not apply to XSendEvent, but will appear the first time any XTEST is used on the desktop display; pre-empt by documenting the Plasma 6.4+ "allow without asking" setting.
- **Modifier desync**: injected modifier state per target is independent of the physical modifier state the focused window sees; the engine must reset target modifier state on toggle-off and on target removal.
- **Anti-cheat**: nothing here defeats device whitelisting or kernel-level anti-cheat, and it should not try to.
- **Game policies**: Blizzard (WoW, since 2020) and CCP (EVE, since 2015) treat input broadcasting as actionable. Stated once; the user decides the target game.

---

## 13. Sources

### Wine and Proton source
- https://github.com/wine-mirror/wine/blob/master/dlls/winex11.drv/event.c
- https://github.com/wine-mirror/wine/blob/master/dlls/winex11.drv/keyboard.c
- https://github.com/wine-mirror/wine/blob/master/dlls/winex11.drv/window.c
- https://github.com/wine-mirror/wine/blob/master/dlls/winex11.drv/mouse.c
- https://github.com/wine-mirror/wine/blob/master/server/queue.c
- https://github.com/wine-mirror/wine/blob/master/dlls/win32u/input.c
- https://github.com/wine-mirror/wine/blob/master/dlls/dinput/dinput_main.c
- https://github.com/wine-mirror/wine/blob/master/dlls/dinput/keyboard.c
- https://github.com/wine-mirror/wine/blob/master/dlls/winewayland.drv/wayland_keyboard.c
- https://github.com/wine-mirror/wine/blob/master/dlls/ws2_32/socket.c
- https://github.com/wine-mirror/wine/blob/wine-10.0/ANNOUNCE.md
- https://github.com/wine-mirror/wine/blob/wine-11.0/ANNOUNCE.md
- https://github.com/ValveSoftware/wine/blob/proton_10.0/dlls/winex11.drv/event.c
- https://github.com/ValveSoftware/wine/blob/proton_10.0/dlls/winex11.drv/mouse.c
- https://github.com/ValveSoftware/Proton/blob/proton_11.0/proton
- https://raw.githubusercontent.com/ValveSoftware/Proton/proton_9.0/proton
- https://github.com/GloriousEggroll/proton-ge-custom/blob/master/proton
- https://steamcommunity.com/groups/SteamClientBeta/discussions/3/803470345661193390/
- https://discuss.cachyos.org/t/proton-with-wayland-driver-new-parameters-etc/18930
- https://github.com/wine-staging/wine-staging/blob/master/patches/user32-rawinput-keyboard/definition
- https://www.phoronix.com/news/Wine-10.0-Released
- https://www.phoronix.com/news/Wine-11.0-Released
- https://www.phoronix.com/news/Wine-Staging-10.2
- https://www.linuxcompatible.org/story/wine-staging-1116-adds-afunix-socket-support-and-shader-improvements
- https://linux.die.net/man/1/wine
- https://man.archlinux.org/man/wineserver.1.en
- https://forum.winehq.org/viewtopic.php?t=4135 (snippet only)
- https://forum.winehq.org/viewtopic.php?t=32974 (snippet only)
- https://forum.winehq.org/viewtopic.php?t=27456 (snippet only)
- https://forum.winehq.org/viewtopic.php?t=5938 (snippet only)
- https://forum.winehq.org/viewtopic.php?f=8&t=27595
- https://forum.winehq.org/viewtopic.php?t=32731
- https://forum.winehq.org/viewtopic.php?t=37699
- https://appdb.winehq.org/objectManager.php?sClass=application&sTitle=Browse+Applications&sOrderBy=appName&bAscending=true&sSearch=hotkeynet

### X11, Xwayland, libei, compositors
- https://www.x.org/releases/X11R7.7/doc/xproto/x11protocol.html#requests:SendEvent
- https://github.com/jordansissel/xdotool/blob/master/xdotool.pod
- https://github.com/jordansissel/xdotool/issues/346
- https://www.semicomplete.com/blog/xdotool-and-exploring-wayland-fragmentation/
- https://bbs.archlinux.org/viewtopic.php?id=282231
- https://www.howtogeek.com/125664/how-to-bind-global-hotkeys-to-a-wine-program-under-linux/
- https://gitlab.freedesktop.org/xorg/xserver/-/blob/master/hw/xwayland/xwayland-xtest.c
- https://gitlab.freedesktop.org/xorg/xserver/-/blob/master/hw/xwayland/man/Xwayland.man
- https://man.archlinux.org/man/Xwayland.1
- https://www.phoronix.com/news/XWayland-23.2-Released
- http://who-t.blogspot.com/2026/07/libei-integrations-in-xdg-remotedesktop.html
- http://who-t.blogspot.com/2022/12/libei-opening-portal-doors.html
- https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.RemoteDesktop.html
- https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.GlobalShortcuts.html
- https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.InputCapture.html
- https://gitlab.gnome.org/GNOME/mutter/-/merge_requests/3303
- https://gitlab.gnome.org/GNOME/mutter/-/merge_requests/2628
- https://gitlab.gnome.org/GNOME/mutter/-/work_items/1974
- https://gitlab.gnome.org/GNOME/xdg-desktop-portal-gnome/-/issues/114
- https://release.gnome.org/48/developers/index.html
- https://invent.kde.org/plasma/kwin/-/tree/master/src/plugins/eis
- https://invent.kde.org/plasma/kwin/-/merge_requests/6178
- https://invent.kde.org/plasma/kwin/-/merge_requests/5496
- https://discuss.kde.org/t/remote-control-requested-still-an-issue/24733
- https://github.com/KDE/xdg-desktop-portal-kde/blob/master/src/remotedesktop.cpp
- https://invent.kde.org/plasma/xdg-desktop-portal-kde/-/issues/12
- https://bugs.kde.org/show_bug.cgi?id=480235
- https://blog.davidedmundson.co.uk/blog/whats-happening-in-kde-remote-desktop-improved-unattended-mode-and-more/
- https://github.com/isac322/kwin-mcp
- https://wayland.app/protocols/kde-fake-input
- https://github.com/atx/wtype/issues/29
- https://github.com/jinliu/kdotool/blob/master/README.md
- https://develop.kde.org/docs/plasma/kwin/api/
- https://github.com/cushycush/wdotool

### gamescope
- https://github.com/ValveSoftware/gamescope/blob/master/README.md
- https://github.com/ValveSoftware/gamescope/blob/master/src/wlserver.cpp
- https://gitlab.freedesktop.org/wlroots/wlroots/-/blob/master/xwayland/server.c
- https://wiki.archlinux.org/title/Gamescope
- https://github.com/ChimeraOS/gamescope-session/blob/main/README.md
- https://github.com/sonic2kk/steamtinkerlaunch/wiki/GameScope
- https://github.com/atty303/gstype
- https://github.com/ValveSoftware/gamescope/issues/1902
- https://github.com/ValveSoftware/gamescope/issues/2032
- https://github.com/ValveSoftware/gamescope/issues/203
- https://github.com/ValveSoftware/gamescope/issues/1285
- https://github.com/ValveSoftware/gamescope/issues/1748
- https://github.com/ValveSoftware/gamescope/issues/1460

### Running processes in prefixes, IPC
- https://apple1417.dev/posts/2025-01-01-proton-multiple-game-instances
- https://github.com/jcnils/protonhax
- https://github.com/Matoking/protontricks
- https://github.com/Matoking/protontricks/issues/145
- https://man.archlinux.org/man/umu.1.en
- https://github.com/Open-Wine-Components/umu-launcher/wiki/Frequently-asked-questions-(FAQ)
- https://copr.fedorainfracloud.org/coprs/bazzite-org/bazzite/package/umu-launcher/
- https://github.com/frostworx/steamtinkerlaunch/issues/269
- https://protondex.gg/guides/steam-launch-options
- https://gist.github.com/michaelbutler/f364276f4030c5f449252f2c4d960bd2
- https://steamcommunity.com/sharedfiles/filedetails/?id=3378517770
- https://man.archlinux.org/man/lutris.1.en.txt
- https://github.com/lutris/lutris/issues/4260
- https://docs.usebottles.com/advanced/cli
- https://github.com/Heroic-Games-Launcher/HeroicGamesLauncher/issues/2766
- https://github.com/openglfreak/winestreamproxy
- https://github.com/FyraLabs/outflow
- https://forum.winehq.org/viewtopic.php?t=7449

### Go, Rust, Python, C libraries and Wine compatibility
- https://github.com/holoplot/go-evdev
- https://github.com/jezek/xgb
- https://github.com/jezek/xgbutil
- https://github.com/godbus/dbus
- https://github.com/bendahl/uinput
- https://github.com/gvalkov/golang-evdev
- https://github.com/bnema/libei-go-bindings
- https://github.com/go-vgo/robotgo
- https://raw.githubusercontent.com/golang/sys/master/windows/zsyscall_windows.go
- https://github.com/icio/go-wine-test
- https://groups.google.com/g/golang-nuts/c/Msg1USaNaqM
- https://github.com/golang/go/issues/5831
- https://github.com/rust-lang/rust/issues/128066
- https://crates.io/crates/evdev
- https://crates.io/crates/evdevil
- https://crates.io/crates/x11rb
- https://lib.rs/crates/reis
- https://github.com/bilelmoussaoui/ashpd
- https://docs.rs/enigo/latest/src/enigo/linux/libei.rs.html
- https://github.com/gvalkov/python-evdev
- https://pypi.org/pypi/python-libei/json
- https://github.com/python-xlib/python-xlib
- https://libinput.pages.freedesktop.org/libei/api/group__liboeffis.html
- https://github.com/jordansissel/xdotool

### Bazzite, permissions, distribution
- https://raw.githubusercontent.com/ublue-os/bazzite/main/Containerfile
- https://github.com/ublue-os/bazzite/blob/main/README.md
- https://github.com/ublue-os/bazzite/issues/2516
- https://github.com/ublue-os/bazzite/issues/3817
- https://docs.bazzite.gg/Advanced/add-user-to-group/
- https://docs.bazzite.gg/Installing_and_Managing_Software/
- https://docs.bazzite.gg/Installing_and_Managing_Software/Homebrew/
- https://docs.bazzite.gg/Installing_and_Managing_Software/rpm-ostree/
- https://docs.bazzite.gg/Gaming/Game_Launchers/
- https://docs.bazzite.gg/Handheld_and_HTPC_edition/quirks/
- https://universal-blue.discourse.group/t/brew-is-now-installed-by-default-in-aurora-bluefin-and-bazzite/1717
- https://universal-blue.discourse.group/t/steam-rpm-fusion-included-or-flatpak/1639
- https://github.com/ValveSoftware/steam-devices/blob/master/60-steam-input.rules
- https://raw.githubusercontent.com/systemd/systemd/main/rules.d/50-udev-default.rules.in
- https://github.com/jtroo/kanata/wiki/Avoid-using-sudo-on-Linux
- https://github.com/ReimuNotMoe/ydotool
- https://github.com/ReimuNotMoe/ydotool/issues/210
- https://sr.ht/~geb/dotool/
- https://wiki.archlinux.org/title/Input_remap_utilities
- https://gitlab.com/CalcProgrammer1/OpenRGB/-/merge_requests/3003
- https://discussion.fedoraproject.org/t/silverblue-solution-for-upstream-udev-rules-steam-devices-openrgb/35576

### HotkeyNet, multiboxing tools, policy
- https://github.com/MacHu-GWU/hotkeynet-project
- https://pypi.org/project/hotkeynet/
- https://hotkeynet.readthedocs.io/
- https://hotkeynet.readthedocs.io/stable/02-Reference/ComparisonChartOfSendModes/index.html
- https://hotkeynet.readthedocs.io/0.1.7/03-Instructions/10-Sending-to-Backgroud-Windows/index.html
- https://hotkeynet.readthedocs.io/0.1.7/03-Instructions/09-Sending-Keystrokes-to-Windows/index.html
- https://hotkeynet.readthedocs.io/0.1.5/02-Reference/SendPC/
- https://www.dual-boxing.com/threads/55349-Multiboxing-on-Linux
- https://www.dual-boxing.com/threads/58194-Tips-WINE-virtual-networks-and-IP-forcing-(ft-praise-for-HKN)
- https://www.dual-boxing.com/threads/16177-Guide-HowTo-use-HotKeyNet-for-boxing
- https://github.com/godlike64/multiwow
- https://github.com/Iiridayn/gw2-linux-multibox-launcher
- https://github.com/arsin305/eve-o-preview-linux
- https://github.com/OpenMultiBoxing/OpenMultiBoxing
- https://github.com/mostlynick3/NickBox
- https://www.autohotkey.com/board/topic/71475-post-your-experience-with-ahk-under-wine-here/
- https://www.autohotkey.com/boards/viewtopic.php?t=65552
- https://blizzardwatch.com/2020/11/12/blizzard-ban-multiboxing-wow/
- https://us.forums.blizzard.com/en/wow/t/policy-update-for-input-broadcasting-may-2021/956610
- https://nosygamer.blogspot.com/2014/12/why-change-key-rebroadcasting-in-eve.html
