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
*Superseded as the intended mechanism by 4.11*: the dance steals real focus from the master client on every broadcast key, which is disqualifying for multiboxing. A no-focus-change path (in-bottle `PostMessage`) is proven; the dance remains the fallback for non-Wine / non-PostMessage-able targets.
*Revisit if.* Measurements on Bazzite show a better default.

**4.3 Only Down and Up are broadcast; auto-repeat events pass through to the focused window only.** (2026-09-07)
Juggling focus at repeat rate is slow and pointless: each target generates its own repeats from the Down it received.

**4.4 The engine runs in a single goroutine and is the only caller of the injector and window manager.** (2026-09-07)
This is the correctness guarantee that two keys' focus dances never interleave. The UI mutates targets/filter/enabled under a mutex; it never touches the backends directly.
*Superseded by 4.9*: passthrough moved off this goroutine so it can't be blocked by a dance. The "never interleave" guarantee still holds, just for the WindowManager and for the dance's own Emit calls specifically, not literally "everything runs on one goroutine" anymore.

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

**4.9 Passthrough (step 1) waits only for a dance that is actually in flight right now — never for the queue behind it, and never fires while one holds real focus elsewhere.** (2026-09-08, corrected same day)
First real-Bazzite-hardware finding (see 7.8): with 2+ targets and real key-cast/movement timing in WoW, the tool was unusable — every keystroke's passthrough had to wait for the *previous* keystroke's entire multi-window dance (`Activate`+`SettleDelay`+`Emit` per target, easily 100ms+ with two targets) to finish, since `Run` processed events one at a time and did the dance inline. First fix attempt: moved the dance to a dedicated worker goroutine (`broadcastLoop`) fed by a bounded channel (`queue`, cap 16, drops the newest event and notifies rather than ever blocking `Run`), with `Run` doing step 1 immediately and a non-blocking enqueue for step 2.
That first attempt was incomplete and shipped a worse bug: passthrough now fired *immediately*, with no regard for whether a dance was actively holding real focus on some other window at that exact moment. Typing during an in-flight dance — the totally ordinary case of chatting while movement/ability keys are also broadcasting — could get a keystroke's passthrough delivered to whatever window the dance currently had focused, not to the window being typed into. Reported as "even the source misses its own keystrokes" and "broadcasts never arrive despite the log saying they did" — both are what misdirected passthrough looks like from the outside, not a delivery failure the log would ever catch, since the engine's own bookkeeping (which window is "origin") was internally consistent the whole time.
Corrected fix: `focusMu` is held by a dance for its *entire* duration — from before it reads the origin window through the final restore — not just around individual `Emit` calls, and passthrough's `Emit` takes the same lock for its one call. This makes "the origin window has real focus" true whenever passthrough actually fires: either no dance is running, or the most recent one has already restored it. Passthrough still never waits for the *backlog* of queued dances — only for whichever single one is in flight, which is what keeps 4.9's original latency fix intact. Regression test: `TestPassthroughNeverFiresWhileADanceHoldsFocus`, which asserts ordering (passthrough's log entry must come after the dance's own restore entry) rather than just presence — confirmed to actually fail against the first, buggy version before landing this one.
Known remaining trade-off, not a bug: under *sustained* back-to-back broadcasts (e.g. movement keys held while also typing, both in the filter), passthrough can still see repeated waits, since each new queued dance can re-acquire `focusMu` before a waiting passthrough gets scheduled. Bounded per-wait, not compounding the way the original bug did, but not eliminated — inherent to "focus can only be in one place," per 4.2's "every Linux multiboxer hits the same wall."
*Also found — see 4.10 for the actual fix*: while investigating this, KWin's scripting `workspace.activeWindow = target` was observed to report success (and have KWin's own `workspace.activeWindow` agree) while XWayland's real X11-level input focus never actually followed — confirmed independently via `xdotool getactivewindow`, invisible to `Active()` since it only ever asks KWin. First characterised as a rare, hard-to-reproduce fluke (once in ~50+ isolated trials on an idle system); turned out to be neither rare nor a fluke — see 4.10. Diagnostic tooling: `cmd/kwindiag` (`list`, `bounce`, `dance` subcommands — KWin-only, no real key injection) and `cmd/e2ediag` (drives the real evdev+kwin+engine stack end to end via a throwaway synthetic keyboard device — this is what caught both the corrected-fix bug above and 4.10's real severity). Neither is wired into `make`; both are real tools, not throwaway scripts.

**4.10 `kwin.WM.Activate` verifies the switch against real X11 focus (via `xdotool`) before returning, retrying if it hasn't happened yet — not just trusting KWin's own report.** (2026-09-08)
The desync noted in 4.9 turned out to be the actual dominant cause of "keys don't arrive," not a minor residual one: live user testing against real WoW/Bottles windows, under real gameplay (not the idle system the earlier isolated KWin-only tests ran on), showed it happening on essentially *every* delivery to certain targets — 100% of broadcasts logged as delivered with zero errors, while the targets never reacted at all. The isolated `bounce`/`dance` tests kept coming back clean because an idle system with no rendering load apparently isn't representative; real GPU/CPU contention from actually running the games appears to slow XWayland's focus propagation enough to matter.
Fix: `Activate` now, when `xdotool` is on PATH, checks `xdotool getactivewindow getwindowname` against the target's own caption (from KWin) after switching, and — if it doesn't match yet — re-issues the KWin activation and checks again, up to 20 attempts at 30ms apart (600ms worst case, comfortably inside Engine's 2s `OpTimeout`). Only returns success once X11 itself confirms the switch; returns a real, visible error (surfaced in the engine's notices, e.g. "activate ...: KWin reports focus switched but real X11 input focus never followed after 20 attempts") if it never does. This applies to every `Activate` call a dance makes, including the final restore-to-origin — which is what explained "even the source misses its own keystrokes" in 4.9: if restore silently failed the same way, every subsequent passthrough correctly waited for focusMu and then correctly emitted to "origin," but real focus had never actually returned there.
Tuning history: the first attempt used 5 attempts at 20ms (100ms budget) and was caught, by this fix's own verification, failing on real windows under real load — confirmed by cross-checking externally via `e2ediag` that real focus *did* land, just after ~300ms, longer than the internal budget allowed. Widened to the current 20×30ms after that measurement. 20 rounds of real end-to-end injection at a realistic ~250ms ability-rotation pace, target-side, came back completely clean with the wider budget.
Caption-based matching (not PID): PID was tried first and rejected — `xdotool getwindowpid` was observed returning bogus values for these same Wine/Proton windows, matching dark-portal's own documented finding about bogus `_NET_WM_PID` from Bottles' runner. Caption matching isn't perfect in general (two unrelated windows could share a title) but is reliable for clonecast's actual use case, since multiboxed clients are each given a distinct title (e.g. by dark-portal's title-keeper).
Without `xdotool` on PATH, `Activate` silently falls back to trusting KWin alone — the pre-4.10 behavior — so this doesn't add a hard new dependency, only a soft one worth having.
`scripts/setup-bazzite.sh` now checks for `xdotool` and prints a distro-appropriate install hint (not installed automatically — rpm-ostree layering needs a reboot, not worth springing on someone for an optional dependency); README's Requirements section notes it too.
*Revisit if.* A future KWin scripting API exposes something more direct than a caption string to verify against (a real X11 XID, if one ever gets exposed on the Window object — checked, not currently there).

**4.11 The focus dance is a dead end for real multiboxing; keys can instead be delivered to an *unfocused* WoW window via a Win32 `PostMessage` from inside the bottle. Proven, not yet integrated.** (2026-09-09)
The dance (4.2/4.10) was made *reliable* — see the `UseTakeFocus` finding below — but reliability was never the real problem. The dance's cost is that it moves real OS keyboard focus off the window you're playing on for every broadcast key. For actual multiboxing that is disqualifying: the master client must never lose focus except when the user chooses. So the goal changed from "make the dance reliable" to "deliver keys without any focus change at all." This entry records what was ruled out and the mechanism that finally worked.

*The Wayland wall, restated.* Input on Wayland goes to whatever surface holds real compositor keyboard focus, full stop. From the Linux side there is no way to hand a key to an unfocused Wine window: `XSendEvent` (synthetic, `send_event=1`) is ignored by the game, and `XTEST` only ever reaches the focused window. So *any* Linux-side mechanism reduces to "give the window focus first" — i.e. the dance.

*Focus mechanisms tried, all insufficient.* Three ways to move focus programmatically were tested against real WoW/Bottles windows, each verified by a visually-unmistakable key (`space` = jump): KWin scripting `workspace.activeWindow` (4.10), `xdotool windowfocus` (raw `XSetInputFocus`), and `xdotool windowactivate` (EWMH `_NET_ACTIVE_WINDOW`). All three update their respective X11/WM bookkeeping and report success, and getwindowfocus/getactivewindow confirm it — yet the injected key still frequently did not register in-game. Root cause: those calls move X11-compat bookkeeping, not KWin's actual Wayland-level keyboard-focus assignment to the surface, which (under the default click-to-focus policy) genuinely only moves in response to a real click. A synthetic **mouse click** on the target *did* reliably deliver — but a click moves the shared cursor and steals real focus, i.e. the dance with extra side effects. Dead end.

*`UseTakeFocus` — real, kept, but not the answer.* One genuine bug was found and fixed along the way: the Bottles bottles had `HKCU\Software\Wine\X11 Driver\UseTakeFocus` unset (default off), so Wine never participated in ICCCM `WM_TAKE_FOCUS` negotiation and programmatic focus never fully "took." Setting it to `Y` (via `bottles-cli reg add`, applied per bottle) made plain `xdotool windowfocus` land keys reliably, 3/3, with zero cursor movement. Worth keeping for anything that *does* rely on focus. It does not change the verdict: it makes the dance reliable, but the dance still steals master focus.

*The mechanism that works: `PostMessage` from inside the bottle.* WoW under Wine **does** read background window messages. A Win32 `PostMessage(hwnd, WM_KEYDOWN/UP, vk, lparam)` delivered to the game's window from a program running *inside the same Wine prefix* reaches the game with **no focus change and no cursor movement**. Proven with HotkeyNet (a mature, native-Win32 multiboxing tool) driving its `SendWinM` instruction (which is exactly this PostMessage): both Malahmen and Marx were made to jump repeatedly while unfocused, visually confirmed, both clients staying healthy. Key details learned:
- Target the **Win32 window title `"World of Warcraft"`** (quoted, since it has spaces), *not* the KWin/X11 title dark-portal's title-keeper sets ("Malahmen"/"Marx") — HotkeyNet runs inside Wine and searches Win32 titles.
- Each bottle has its own `wineserver`, so a program in one prefix can only see/message windows in that prefix. One HotkeyNet per bottle therefore targets only its own game — the shared `"World of Warcraft"` title never cross-fires. This is *ideal* for multiboxing, and it is also why the title alone doesn't hit "both."
- HotkeyNet's own keyboard hook does **not** work under Wine (its "Last key press" panel never registered a uinput-injected key), so it can't be triggered by a hotkey here. It was triggered instead via `<Command Autoexec>` + `<SendPC Local>` + `<SendWinM "World of Warcraft">` + `<Key ...>`, run on script load / the in-app "Reload script" button. HotkeyNet Commands otherwise relay to the network, not locally — `SendLabel` is forbidden in commands; the direct `SendWinM` form is what works locally.

*What crashes clients (so future work avoids it).* Not `SendWinM`, and not HotkeyNet — both are safe and coexist with the running games indefinitely. The crashes during this investigation came from (a) a **Go-compiled helper doing `FindWindow`/`EnumWindows` inside a live wineserver session** (the Go/Wine callback path spun at 100% CPU or hung, and took the game down with it), and (b) **`pkill -9` (SIGKILL) on any process sharing the game's wineserver** (tears down the wineserver, killing the game too — always close in-bottle processes gracefully). Malahmen was killed three times this way before the cause was isolated.

*Why clonecast can't just call `SendWinM`.* `SendWinM` is a Win32 `PostMessage`; it can only run inside the Wine prefix. clonecast is a native Linux process and cannot make Win32 calls into a bottle from outside (and the X11 route only reaches the focused window). So the delivery half is solved, but it requires an **in-bottle Windows agent** that does the PostMessage, with clonecast triggering it in real time. That bridge is the remaining build — see 7.9.

**4.12 `XSendEvent` (Technique A) works only in Wine virtual-desktop mode and sticks keys; PostMessage (Technique B) is clean. Decision: build both as optional, selectable delivery backends, B as the reliable default.** (2026-09-09)
A commissioned research doc (`research.md`, root of repo) argued from Wine source that the *primary* mechanism should be `XSendEvent` sent from Linux straight to the game's X window (Technique A): winex11.drv ignores the `send_event` flag, so a synthetic X key is routed to the addressed window's thread focus even unfocused, and — unlike PostMessage — key state / Raw Input / hooks are all synthesized from it, reaching DirectInput/Raw-Input games too. This session tested that claim empirically on the real box, against real WoW/Bottles clients, with the user confirming visually (space = jump). Results:
- **Windowed (non-virtual-desktop) WoW: XSendEvent does NOT reach a background window.** Tested the top-level, the client child, and the "Default IME" window; via both `xdotool key --window` and a direct `jezek/xgb` probe with the exact event fields the research specifies. Nothing landed. This matches a caveat the research itself flagged (the only known background-XSendEvent successes were with clients in a Wine virtual desktop; `multiwow` hard-requires it).
- **Virtual-desktop mode: XSendEvent WORKS.** With one client (Almerinda) relaunched under a Wine virtual desktop (`HKCU\Software\Wine\Explorer` `Desktop`="Default" + `Desktops\Default`="1024x768"), a direct-xgb XSendEvent of space to the in-VD "World of Warcraft" child window made it jump while another client held focus. So the VD structure (a real child X window inside the VD's X window) is what routes correctly; plain windowed does not.
- **Two problems with A even in VD mode.** (1) A *reproducible stuck key* — after sending space, a key stays engaged (character runs / keeps jumping); persisted even after fixing the probe (monotonic timestamps, real 50ms hold, per-event flush, guaranteed final release), and **confirmed independent of HotkeyNet** (still reproduced with every HotkeyNet process killed — see the isolation below). Inherent-looking: XSendEvent's power comes from Wine synthesizing *hardware key state* from the event (that's how it reaches Raw-Input games), and that same synthesized state isn't cleared by a release delivered to a *background* window — the discrete jump on KeyPress works, but the polled key state WoW's movement reads stays "down." (2) An apparent focus-gain, traced to our own `xdotool windowactivate` setup call, **not** XSendEvent — gone once we stopped calling it.
- **Isolation of a *separate* stuck-key cause: HotkeyNet's keyboard hook.** Mid-investigation the user noticed the game stuck keys even from the *physical* keyboard. Killing every HotkeyNet process (gracefully — `SIGTERM`, never `SIGKILL`, which tears down the shared wineserver and the game with it) fixed physical/focused input immediately, with `UseTakeFocus` still `Y` (so `UseTakeFocus` is exonerated). So HotkeyNet's `WH_KEYBOARD_LL` hook — well-known to misbehave under Wine — mangles real keyboard input for the whole Wine session while it runs. This is a distinct bug from A's background-XSendEvent stick (that one persists with HotkeyNet dead), and it is a concrete reason the `agent` backend must be a **custom hookless helper, not HotkeyNet**: our agent only ever calls `PostMessage`, installs no hook, and so cannot corrupt the user's live typing the way a running HotkeyNet does.
- **PostMessage (Technique B, proven in 4.11) was clean on every count**: discrete jumps, no stuck keys, no focus change, and it works in dark-portal's *normal windowed mode* with no VD change. For vanilla WoW (which reads `WM_KEYDOWN`) B's only theoretical weakness — not reaching Raw-Input-only games — does not apply.

Decision: delivery becomes a **pluggable backend** chosen per run (later per target). Three backends: `agent` (Technique B — TCP to an in-bottle PostMessage helper; the reliable default), `xsend` (Technique A — `jezek/xgb` XSendEvent; *experimental*, requires the client in VD mode and still has the stuck-key caveat, but is pure-Linux and the only path that reaches Raw-Input games), and `dance` (the deprecated KWin focus-dance, kept only as a last-resort fallback for non-Wine targets). This mirrors `research.md` §10's pluggable design. Phasing: interface first, then `agent`, then `xsend`. See 7.9.

**4.13 The `agent` backend is built with our own code and proven clean end-to-end on two clients.** (2026-09-09) `cmd/clonecast-agent` (Go, `GOOS=windows GOARCH=386`) runs inside each bottle: it locates the game window once via a **`GetWindow` tree-walk** (callback-free — no `EnumWindows` spin, no `FindWindow` hang; it found the window in a live wineserver with no hang, the thing our earlier Go probes couldn't do), caches the HWND, listens on a loopback TCP port, and `PostMessage`s `WM_KEYDOWN/UP` per frame — **no keyboard hook** (so it can't corrupt live typing the way HotkeyNet did, 4.12). `internal/agentwire` is the shared protocol: newline frames carrying Linux evdev codes, plus the evdev→Win32 VK/scancode map — small because a Linux evdev code numerically equals the PC Set-1 make scancode for the whole main block (KEY_SPACE=57=0x39, KEY_A=30=0x1E, …), so only the virtual-key needs a table and only E0-extended keys (arrows) need an explicit scancode. `internal/platform/agent` is the Linux-side `broadcast.Deliverer` (focus-free): persistent loopback connections per target, evdev frames out. Verified: Wine's Winsock listener on `127.0.0.1:<port>` is reachable from the Linux host (`ss` shows it owned by wineserver, shared loopback), and sending Space frames made both Malahmen and Marx jump — discrete, no stuck key, no focus change, each agent hitting only its own prefix's window. Remaining to ship: a `--backend` selector + per-instance endpoint discovery in `cmd/clonecast`, dark-portal launching one agent per instance (like its title-keeper) on a known per-instance port, and a full physical-keyboard multibox pipeline test.

**4.14 Full pipeline proven through the real binary; agent hardened; lifecycle must be instance-owned.** (2026-09-09) `clonecast --deliver agent --agent "Marx=48901,..."` was driven end-to-end on real clients: pressing a broadcast key on the focused master made the follower (Marx) mirror it via evdev capture → engine → `agentDeliverer` → loopback TCP → in-bottle agent → `PostMessage`, with the master reacting through passthrough and **focus never moving**. Selection is `--deliver dance|agent|xsend`; `--agent` maps a target's window title to its agent endpoint (title→"host:port"), resolved through a short-lived title cache over the WindowManager. Two hardening fixes were needed and are in place:
- **The Go agent nondeterministically spun at ~100% CPU under Wine** (one agent came up fine, a second spun). Fix: `runtime.GOMAXPROCS(1)` at startup — Go's multi-P scheduler/sysmon is what spins under Wine; one P removes it. Stable since.
- **A once-cached HWND goes stale** because WoW recreates its top-level window across state changes (login→world, loading). Fix: the agent validates the cached HWND (`IsWindow` + title match) each send and re-walks (`GetWindow` tree) only when stale. Also the Linux `agentDeliverer` retries a send once through a fresh dial (a "connection reset" from a restarted agent no longer drops a keystroke), and warns only once about a ticked-but-unmapped target instead of per key.
- **Lifecycle: never externally kill a co-prefix agent.** Killing the agent process — even `SIGTERM` to just the `.exe`, and worse its bash launcher — tears down the wineserver it shares with the game and kills the game (cost three client deaths this session). The agent must be launched by dark-portal **as part of the instance** so it lives and dies with that game's own session and is never signalled from outside. That is the remaining integration step, plus the dark-portal instance↔process naming race the user spotted (stopping one instance killed another's window — a dark-portal pidfile/title-keeper bug, tracked separately).

**4.15 The master is the focused window, resolved fresh per broadcast — implements 7.10.** (2026-09-10) Before this, only `danceDeliverer` resolved and skipped `origin` (`wm.Active`); `agentDeliverer` and the `xsend` deliverer broadcast to every ticked target unconditionally, so ticking the currently-focused window alongside its peers — the natural way to use "dynamic master" — delivered the key to it **twice** (once via passthrough, once via the backend). Fixed at the engine, not per-backend: `Deliverer.Deliver` gained an `origin WindowID` parameter; `Engine.broadcast` resolves `origin := e.wm.Active(opCtx)` once per broadcast key and passes it to whichever deliverer is active, so every backend — not just the dance — skips it uniformly. `danceDeliverer` no longer calls `wm.Active` itself; it just uses the origin it's handed (one round trip instead of two per key). `agentDeliverer` and the `xsend` deliverer both gained the same one-line `if t == origin { continue }`.
Also added `Config.RequireOriginTicked` (default `true`, the research's recommended gate): if the focused window isn't itself one of the ticked targets — you tabbed to the terminal, a browser, anything not part of the multibox session — broadcast is suppressed entirely for that key, even with broadcasting on and the key in the filter; passthrough still fires. This is a free win on 7.2's still-open "own-window exclusion" problem: if you never tick the terminal clonecast runs in, focusing it now silently stops broadcast without any PID-walking own-window detection. Set `RequireOriginTicked: false` to restore the pre-4.15 "always broadcast to every ticked target" behavior.
Tests: `TestDynamicMasterOriginSkippedByNonDanceBackend` is the regression test — a fake focus-free `Deliverer` records the `origin`/`targets` it's handed; verified genuine by injecting a broken origin resolution (hardcoding `origin := WindowID("")`) and confirming the test fails, then reverting. `TestBroadcastGatedWhenOriginNotTicked` and `TestBroadcastGateCanBeDisabled` cover the new gate both ways. All pre-existing tests pass unchanged except `TestPassthroughNeverFiresWhileADanceHoldsFocus`, which now ticks `origin` alongside its target so the new default gate doesn't suppress the dance it's testing — the gate itself isn't what that test is about.
Deliberately not done here: the agent-side foreground-check safety net (`GetForegroundWindow`/`WinEventHook` inside the prefix, C1's second half in `research.md`) — that needs rebuilding and live-testing the Windows agent against real clients, which is real risk after this session's three prefix-related client deaths; the engine-side fix alone already closes the double-delivery bug and delivers 7.10's UX. Also not done: flipping `--deliver`'s default from `dance` to `agent` (research I11) — `agent` still requires a hand-written `--agent` endpoint map (7.12/O5 unsolved), so defaulting to it would break a zero-flag `clonecast` invocation; revisit once self-registration (7.12) removes that requirement.

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

**7.6 Performance of per-operation KWin scripts.** Measured on real Bazzite hardware (2026-09-08): `List` ~3ms, `Active` ~1ms, `Activate` ~5-10ms per call — not the bottleneck. See 4.9 for what actually was (passthrough blocking on the dance) and for a related, separate finding (an intermittent KWin/XWayland focus desync). See 3.3 for the scripting-overhead fallback plan, now lower priority given these numbers.

**7.7 License.** Not chosen. Owner's call.

**7.9 Pluggable delivery backends (the build after 4.11/4.12).** (2026-09-09) Delivery becomes an interface with the method chosen per run (later per target); the engine's hardwired dance in `broadcast()` is replaced by a call to the selected backend. A backend either moves real focus (the dance) or does not (agent/xsend) — the engine only needs its `focusMu`/origin-restore coordination for the former, so a focus-free backend lets passthrough never wait. Three backends, phased in this order:
- **`agent` (Technique B — the reliable default).** A tiny custom Windows helper, one per bottle, listening on a localhost TCP socket (Winsock works under Wine); on each received key it does `PostMessage(WM_KEYDOWN/UP)` to the WoW window (resolve the HWND **once** at startup and cache it — a Go helper hung doing `FindWindow` repeatedly in a live session per 4.11; pure `PostMessage` on a known HWND was never implicated; or write it in C/mingw to sidestep Go/Wine callback issues). clonecast fans keys out over TCP. Launch it per instance the way dark-portal launches the title-keeper; never SIGKILL it (4.11). Proven clean for WoW. (Rejected alt: driving HotkeyNet — its network trigger protocol is undocumented and its keyboard hook doesn't work under Wine.)
- **`xsend` (Technique A — experimental, optional).** `internal/platform/x11` on `jezek/xgb`: build `KeyPress`/`KeyRelease` with `Detail=evdevcode+8`, `SameScreen=true`, tracked-modifier `State`, and `xproto.SendEvent`. Pure-Linux, no agent, and the only backend that reaches Raw-Input/DirectInput games. Ships behind an explicit opt-in because (4.12) it requires the client in Wine virtual-desktop mode **and** still has an unsolved stuck-key issue (Wine's synthesized hardware key-state isn't cleared by a release to a background window). Do not make it the default until the stuck key is solved.
- **`dance`** — the existing KWin focus-dance, demoted to last-resort fallback for non-Wine targets that can't take either of the above.
Open sub-questions (all backends): window identity — targets are currently KWin UUIDs, but `xsend` needs X window ids and `agent` needs a per-bottle endpoint; `research.md` §10 suggests listing from X (`_NET_CLIENT_LIST`) so targets are X windows directly. Held keys / auto-repeat semantics; modifier sequencing; how clonecast discovers each bottle's `agent` port (fixed per-instance port, or a handshake).

Build constraints discovered 2026-09-09:
- **No mingw/zig on the box**, so the `agent` cannot be compiled to native C without rpm-ostree layering (a reboot) or a container. Build it in **Go (`GOOS=windows CGO_ENABLED=0`)** instead — but avoid the two Windows calls that misbehaved under Wine in a live wineserver: `EnumWindows` (its `syscall.NewCallback` trampoline spun at ~100% CPU) and `FindWindowW` (hung when a busy WoW shared the wineserver). The proven-safe way to locate the game window from Go is a **`GetWindow` tree-walk** (`GetDesktopWindow` → `GW_CHILD` → `GW_HWNDNEXT`, reading `GetWindowTextW` per window) — callback-free and observed not to hang — done once at startup to cache the HWND; then only `PostMessageW` on the cached HWND (never implicated in a hang). Load these via `NewLazySystemDLL("user32.dll")` since `x/sys/windows` omits them.
- **HotkeyNet is not usable as the agent** even though it proved the mechanism: its `WH_KEYBOARD_LL` hook corrupts the user's live physical typing for the whole Wine session while it runs (4.12). The custom agent installs no hook.
- **`xsend` is buildable today** with `jezek/xgb` (the probe already sends working XSendEvent), no Wine-side component — it's the lower-tooling-risk of the two to implement, gated experimental by its background stuck-key (4.12).

**7.8 Nothing Linux-specific has run on hardware yet.** (updated 2026-09-08) Partially resolved: built and run for real on Bazzite. `setup-bazzite.sh`'s `usermod -aG input` turned out to silently no-op on this host (the `input` group is image-provided, only in `/usr/lib/group`, not `/etc/group` — same class of issue as Docker's group on rpm-ostree hosts); fixed with a verify-and-fall-back-to-a-local-override, same shape as the fix already proven for `docker`. With that cleared, found and fixed the passthrough-blocking issue in 4.9, and found (but didn't fully resolve) the KWin/XWayland focus desync also noted there. Still not done: the official smoke test (README, two plain editors) hasn't been run to isolate whether that desync is Wine/XWayland-specific or general.

**7.10 The "master" should be the currently-focused window, not a fixed one.** (2026-09-09, owner request; **implemented 2026-09-10, see 4.15**) Today you tick specific followers and play a fixed master. But if the master char dies mid-combat you must switch to another client to watch the fight — and that client should then become the master (receive your real input via passthrough) while the former master becomes a follower. Design: tick *all* your clients as targets, and treat whichever window is focused right now as the master — i.e. the broadcast set is "all ticked targets except the currently-focused one." 4.15 does exactly this at the engine level (origin resolved once per key, threaded to every deliverer, all three skip it) plus a gate so an unticked focused window suppresses broadcast entirely. Not yet done: `research.md`'s agent-side safety net (an in-prefix foreground check via `GetForegroundWindow`/`WinEventHook`, to cover the race window between the engine's `wm.Active()` call and actual delivery) — the engine-side fix is the correctness-critical half and stands alone; the agent-side half is a live-tested hardening layer for later.

**7.11 Decouple the agent from dark-portal.** (2026-09-09, owner request) Launching the agent from dark-portal (7.9's remaining step) couples clonecast's broadcast to dark-portal and makes it WoW/dark-portal-specific, when clonecast should stay a generic broadcaster for any game/software. Better: keep the agent launch generic — e.g. clonecast (or a small standalone launcher) starts the agent into a prefix given only a prefix path + Wine runner (no dark-portal knowledge), or the agent is shipped/launched by whatever owns the game with a documented contract. The hard constraint is only that the agent must run *inside* the target's Wine prefix; nothing about that requires dark-portal specifically. Find a decoupled launch path.

**7.12 Target selection (TUI tick) and endpoint mapping (static `--agent` ports) are redundant and counter-intuitive.** (2026-09-09, owner request) You currently both tick a window in the TUI *and* separately hand clonecast a static title→port map. That's two mechanisms for "which targets," and static per-instance ports feel arbitrary. Unify them: the agent should **self-register** — on startup announce its window identity (title/class/pid) and port to clonecast over a known rendezvous (a small local registry file, a fixed control port, or a broadcast), so the TUI simply lists windows that have a live agent and ticking one is all that's needed, with no hand-written port map. Ties into 7.9's endpoint-discovery decision (CLI flag was chosen for decoupling; this is the UX cost, now to be paid down).

---

## 8. Security and privacy notes

- Grabbing the keyboard means clonecast sees every keystroke, including passwords. Passthrough is not logged. Broadcast events are logged as key names (e.g. `A down -> 2 target(s)`) while broadcasting is on. Turn broadcasting off before typing secrets.
- The DBus name `org.malahmen.clonecast` is claimed exclusively; a second instance fails to start rather than fighting over callbacks.
- KWin scripts are written to a private temp dir (`0600`) and removed after each run.
