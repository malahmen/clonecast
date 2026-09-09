// Command kwindiag is a standalone debugging tool for clonecast's KWin
// backend (internal/platform/kwin). It talks to KWin's scripting D-Bus
// interface directly — the same mechanism kwin.WM uses — without going
// through the broadcast engine, so window listing and focus behaviour can be
// inspected and measured in isolation.
//
// Built while chasing REFERENCE.md 4.9's two findings: it confirmed
// per-operation KWin script overhead is not the bottleneck (List/Active/
// Activate all land in single-digit milliseconds), and it's what surfaced
// the intermittent KWin/XWayland focus desync noted there — cases where
// KWin's own `workspace.activeWindow` agrees a switch happened while
// XWayland's real X11-level input focus (independently checked via
// `xdotool getactivewindow`) never actually followed. That desync has not
// been made to reproduce on demand; `bounce` exists to keep trying.
//
// Usage:
//
//	kwindiag list
//	    Lists every normal, non-minimised window KWin reports: internalId,
//	    title, resource class, pid, and whether it's currently active. IDs
//	    are what `bounce` (and clonecast itself) key windows by.
//
//	kwindiag bounce <id1> <id2> [rounds] [settleMs]
//	    Repeatedly activates id1, settles, then activates id2 and checks
//	    three things: whether the KWin script call itself errored, whether
//	    KWin's own workspace.activeWindow agrees, and — via `xdotool
//	    getactivewindow`, independent of KWin — whether the real X11/
//	    XWayland focus actually followed. rounds defaults to 20, settleMs to
//	    30 (clonecast's own default SettleDelay). Needs `xdotool` on PATH;
//	    the X11 cross-check is skipped (not failed) without it.
//
//	kwindiag dance <origin> <target> [rounds] [settleMs]
//	    Replays the engine's own broadcast.go shape exactly, once per round:
//	    activate target, settle, activate origin (the restore step), settle,
//	    checking real X11 focus (xdotool) after *each* activation, not just
//	    at the end. Built to test a specific question bounce alone can't
//	    answer: does the restore-to-origin step ever silently fail the same
//	    way a switch-to-target can, and if it does, does X11 focus get stuck
//	    on target for good afterwards (rather than resolving on its own) —
//	    which would explain "the origin itself misses keystrokes" and
//	    "broadcasts never arrive despite the log saying they did" as one
//	    mechanism: once desynced, every later Active()/Activate() call the
//	    real engine makes is reasoning from KWin's self-consistent but wrong
//	    idea of what has focus, not from what X11 is actually delivering
//	    input to.
//
// Requires a KWin Scripting session (i.e. running inside the real Plasma
// session, not the mock backend) — same requirement as clonecast itself.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	kwinService   = "org.kde.KWin"
	scriptingPath = "/Scripting"
	scriptingIfce = "org.kde.kwin.Scripting"
	scriptIfce    = "org.kde.kwin.Script"

	busName = "org.malahmen.clonecast.kwindiag"
	objPath = "/org/malahmen/clonecast/kwindiag"
	ifce    = "org.malahmen.clonecast.kwindiag"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		fatalf("session bus: %v", err)
	}
	defer conn.Close()
	if reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue); err != nil {
		fatalf("request name: %v", err)
	} else if reply != dbus.RequestNameReplyPrimaryOwner {
		fatalf("%s already owned — another kwindiag running?", busName)
	}
	kw := &kwinClient{conn: conn, pending: make(chan string, 1)}
	if err := conn.Export(&callback{kw}, objPath, ifce); err != nil {
		fatalf("export callback: %v", err)
	}

	switch os.Args[1] {
	case "list":
		cmdList(kw)
	case "bounce":
		cmdBounce(kw, os.Args[2:])
	case "dance":
		cmdDance(kw, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  kwindiag list")
	fmt.Fprintln(os.Stderr, "  kwindiag bounce <id1> <id2> [rounds] [settleMs]")
	fmt.Fprintln(os.Stderr, "  kwindiag dance <origin> <target> [rounds] [settleMs]")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// -----------------------------------------------------------------------
// KWin scripting client — a minimal standalone copy of kwin.WM's mechanism
// (see internal/platform/kwin/kwin.go), kept separate on purpose: this tool
// must keep working even if that implementation changes shape.
// -----------------------------------------------------------------------

type kwinClient struct {
	conn    *dbus.Conn
	seq     int
	pending chan string
}

type callback struct{ kw *kwinClient }

func (c *callback) Result(payload string) *dbus.Error {
	select {
	case c.kw.pending <- payload:
	default:
	}
	return nil
}

// run loads body as a throwaway KWin script, executes it, and waits for its
// one reply() call. Mirrors kwin.WM.run.
func (k *kwinClient) run(ctx context.Context, body string) (string, error) {
	k.seq++
	plugin := fmt.Sprintf("kwindiag-%d-%d", os.Getpid(), k.seq)
	path := fmt.Sprintf("/tmp/%s.js", plugin)
	src := fmt.Sprintf(`function reply(s) { callDBus(%q, %q, %q, "Result", String(s)); }
%s`, busName, objPath, ifce, strings.TrimSpace(body))
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		return "", err
	}
	defer os.Remove(path)

	select {
	case <-k.pending:
	default:
	}

	scripting := k.conn.Object(kwinService, scriptingPath)
	var id int32
	if err := scripting.CallWithContext(ctx, scriptingIfce+".loadScript", 0, path, plugin).Store(&id); err != nil {
		return "", fmt.Errorf("loadScript: %w", err)
	}
	defer scripting.Call(scriptingIfce+".unloadScript", 0, plugin)
	if id < 0 {
		return "", fmt.Errorf("kwin refused script (id %d)", id)
	}

	scriptObj := k.conn.Object(kwinService, dbus.ObjectPath(fmt.Sprintf("%s/Script%d", scriptingPath, id)))
	if err := scriptObj.CallWithContext(ctx, scriptIfce+".run", 0).Err; err != nil {
		return "", fmt.Errorf("run: %w", err)
	}

	select {
	case out := <-k.pending:
		return out, nil
	case <-ctx.Done():
		return "", fmt.Errorf("kwin script did not reply: %w", ctx.Err())
	}
}

type wire struct {
	ID     string `json:"id"`
	Title  string `json:"caption"`
	Class  string `json:"class"`
	PID    int    `json:"pid"`
	Active bool   `json:"active"`
}

func (k *kwinClient) list(ctx context.Context) ([]wire, error) {
	out, err := k.run(ctx, `
		const wins = workspace.windowList()
			.filter(w => w.normalWindow && !w.skipTaskbar)
			.map(w => ({
				id: String(w.internalId),
				caption: w.caption,
				class: w.resourceClass,
				pid: w.pid,
				active: w === workspace.activeWindow,
			}));
		reply(JSON.stringify(wins));
	`)
	if err != nil {
		return nil, err
	}
	var raw []wire
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("decode window list: %w", err)
	}
	return raw, nil
}

func (k *kwinClient) active(ctx context.Context) (string, error) {
	return k.run(ctx, `reply(workspace.activeWindow ? String(workspace.activeWindow.internalId) : "");`)
}

func (k *kwinClient) activate(ctx context.Context, id string) (string, error) {
	q, _ := json.Marshal(id)
	return k.run(ctx, fmt.Sprintf(`
		const t = workspace.windowList().find(w => String(w.internalId) === %s);
		if (t) { workspace.activeWindow = t; reply("ok"); } else { reply("missing"); }
	`, q))
}

// -----------------------------------------------------------------------
// subcommands
// -----------------------------------------------------------------------

func cmdList(kw *kwinClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t0 := time.Now()
	wins, err := kw.list(ctx)
	if err != nil {
		fatalf("list: %v", err)
	}
	fmt.Printf("List took %v, %d window(s):\n", time.Since(t0), len(wins))
	for _, w := range wins {
		fmt.Printf("  id=%s title=%q class=%q pid=%d focused=%v\n", w.ID, w.Title, w.Class, w.PID, w.Active)
	}
}

// x11ActiveName cross-checks real X11/XWayland input focus independent of
// KWin's own view — see package doc. Returns "" (not an error) if xdotool
// isn't available, so bounce can still run its KWin-only checks.
func x11ActiveName() (string, bool) {
	if _, err := exec.LookPath("xdotool"); err != nil {
		return "", false
	}
	out, err := exec.Command("xdotool", "getactivewindow", "getwindowname").Output()
	if err != nil {
		return "ERR:" + err.Error(), true
	}
	return strings.TrimSpace(string(out)), true
}

func cmdBounce(kw *kwinClient, args []string) {
	if len(args) < 2 {
		usage()
		os.Exit(2)
	}
	id1, id2 := args[0], args[1]
	rounds := 20
	if len(args) > 2 {
		fmt.Sscanf(args[2], "%d", &rounds)
	}
	settleMs := 30
	if len(args) > 3 {
		fmt.Sscanf(args[3], "%d", &settleMs)
	}
	settle := time.Duration(settleMs) * time.Millisecond

	haveX11 := false
	if _, ok := x11ActiveName(); ok {
		haveX11 = true
	} else {
		fmt.Println("xdotool not found on PATH — skipping the X11 cross-check (KWin-only mode).")
	}

	ctx := context.Background()
	origin, err := kw.active(ctx)
	if err != nil {
		fatalf("active: %v", err)
	}

	ids := []string{id1, id2}
	kwinFails := 0
	for i := 0; i < rounds; i++ {
		want := ids[i%2]
		t0 := time.Now()
		out, err := kw.activate(ctx, want)
		elapsed := time.Since(t0)
		time.Sleep(settle)

		kwinActive, _ := kw.active(ctx)
		kwinOK := kwinActive == want
		if !kwinOK {
			kwinFails++
		}

		line := fmt.Sprintf("round %2d: activate(%s) took %v script=%s err=%v kwinOK=%v", i, want, elapsed, out, err, kwinOK)
		if haveX11 {
			x11Name, _ := x11ActiveName()
			// We don't know the target's real window title here, only its
			// KWin internalId, so this is a same/different-from-origin
			// sanity signal, not a title match — good enough to catch the
			// "KWin says yes, X11 says no" desync this tool exists to find.
			line += fmt.Sprintf(" x11Active=%q", x11Name)
		}
		fmt.Println(line)
	}

	fmt.Printf("\nkwin-level failures: %d/%d\n", kwinFails, rounds)
	if haveX11 {
		fmt.Printf("(compare x11Active across rounds by eye: it should change every round — a value that " +
			"repeats across a KWin-claimed switch is exactly the desync in REFERENCE.md 4.9.)\n")
	}

	if origin != "" {
		_, _ = kw.activate(ctx, origin)
	}
}

// cmdDance replays broadcast.go's own dance shape — activate target, settle,
// activate origin (restore), settle — checking real X11 focus after every
// single activation, not just at the end of a round. See the package doc for
// why: this is specifically hunting for the restore step failing the same
// way a switch-to-target can, and for whether X11 focus gets stuck once it
// does, rather than a one-off blip.
func cmdDance(kw *kwinClient, args []string) {
	if len(args) < 2 {
		usage()
		os.Exit(2)
	}
	origin, target := args[0], args[1]
	rounds := 20
	if len(args) > 2 {
		fmt.Sscanf(args[2], "%d", &rounds)
	}
	settleMs := 30
	if len(args) > 3 {
		fmt.Sscanf(args[3], "%d", &settleMs)
	}
	settle := time.Duration(settleMs) * time.Millisecond

	if _, ok := x11ActiveName(); !ok {
		fatalf("xdotool not found on PATH — dance needs it, unlike bounce, since it's specifically checking real X11 focus at every step.")
	}

	ctx := context.Background()
	// Start every round from a known-good state.
	if _, err := kw.activate(ctx, origin); err != nil {
		fatalf("initial activate(origin): %v", err)
	}
	time.Sleep(settle)

	stuck := 0
	lastX11, _ := x11ActiveName()
	for i := 0; i < rounds; i++ {
		// Step: switch to target.
		_, err := kw.activate(ctx, target)
		time.Sleep(settle)
		kwinAfterTarget, _ := kw.active(ctx)
		x11AfterTarget, _ := x11ActiveName()

		// Step: restore to origin — the step bounce never isolates.
		_, rerr := kw.activate(ctx, origin)
		time.Sleep(settle)
		kwinAfterRestore, _ := kw.active(ctx)
		x11AfterRestore, _ := x11ActiveName()

		targetKwinOK := kwinAfterTarget == target
		restoreKwinOK := kwinAfterRestore == origin
		// "Stuck" means X11 didn't move at all across this whole round —
		// the specific, worse failure mode this subcommand exists to catch,
		// as opposed to a transient one-step miss that self-corrects.
		isStuck := x11AfterTarget == lastX11 && x11AfterRestore == lastX11
		if isStuck {
			stuck++
		}
		lastX11 = x11AfterRestore

		fmt.Printf("round %2d: activate(target) err=%v kwinOK=%v x11=%q | activate(origin/restore) err=%v kwinOK=%v x11=%q%s\n",
			i, err, targetKwinOK, x11AfterTarget, rerr, restoreKwinOK, x11AfterRestore,
			map[bool]string{true: "  <-- STUCK (X11 never moved this round)", false: ""}[isStuck])
	}

	fmt.Printf("\n%d/%d rounds never moved real X11 focus at all despite KWin activity.\n", stuck, rounds)
}
