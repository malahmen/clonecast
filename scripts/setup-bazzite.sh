#!/usr/bin/env bash
# One-time permission setup for clonecast on Bazzite (or any Fedora Atomic /
# systemd based distro). Everything it touches lives under /etc, which is
# writable on rpm-ostree systems, so no layering or reboot into a new image is
# needed. Re-running is harmless.
#
# What it does and why (see REFERENCE.md, "Permissions"):
#   1. Adds you to the "input" group  -> read /dev/input/event* (keyboard capture)
#   2. Installs a udev rule            -> /dev/uinput owned by group "input", mode 0660
#   3. Loads the uinput module at boot -> /dev/uinput exists before clonecast starts
set -euo pipefail

if [[ $EUID -eq 0 ]]; then
  echo "run as your normal user; the script calls sudo where needed" >&2
  exit 1
fi

RULE=/etc/udev/rules.d/60-clonecast-uinput.rules
MODCONF=/etc/modules-load.d/clonecast-uinput.conf

echo "==> adding $USER to group 'input'"
# On rpm-ostree/image-based hosts (Bazzite included) "input" is often defined
# only in the read-only /usr/lib/group (baked in via systemd-sysusers), with
# no line in the mutable /etc/group. `usermod -aG` silently no-ops in that
# case (exit 0, nothing added) since it only ever edits /etc/group and never
# creates a line for a group it doesn't find there — verify the result
# instead of trusting usermod's exit code.
sudo usermod -aG input "$USER"
if ! groups "$USER" | grep -qw input; then
  echo "==> usermod didn't take (group 'input' is likely image-provided, not in /etc/group) — adding a local override instead"
  gid="$(getent group input | cut -d: -f3)"
  [[ -n "$gid" ]] || { echo "could not resolve the 'input' group's gid — add $USER to it manually" >&2; exit 1; }
  sudo sh -c "echo 'input:x:${gid}:${USER}' >> /etc/group"
  groups "$USER" | grep -qw input || { echo "still couldn't add $USER to 'input' — add it manually" >&2; exit 1; }
fi

echo "==> installing $RULE"
sudo tee "$RULE" >/dev/null <<'RULES'
# clonecast: let members of "input" create virtual keyboards
KERNEL=="uinput", SUBSYSTEM=="misc", GROUP="input", MODE="0660", OPTIONS+="static_node=uinput"
RULES

echo "==> ensuring uinput loads at boot ($MODCONF)"
echo uinput | sudo tee "$MODCONF" >/dev/null
sudo modprobe uinput || true

echo "==> reloading udev"
sudo udevadm control --reload-rules
sudo udevadm trigger --name-match=uinput || true

# xdotool is optional, not required: without it, Activate() falls back to
# trusting KWin's own report of a focus switch, which REFERENCE.md 4.10
# found is not reliable on its own for real game windows under real load.
# With it, Activate() independently verifies the switch against real X11
# focus and retries if it hasn't happened yet. Recommended, not installed
# automatically here — rpm-ostree layering needs a reboot, which this
# script shouldn't spring on you for an optional dependency.
if ! command -v xdotool &>/dev/null; then
  echo "==> xdotool not found (optional, but recommended — see REFERENCE.md 4.10)"
  if command -v rpm-ostree &>/dev/null; then
    echo "    install with: rpm-ostree install xdotool   (needs a reboot to take effect)"
  elif command -v dnf &>/dev/null; then
    echo "    install with: sudo dnf install xdotool"
  elif command -v apt-get &>/dev/null; then
    echo "    install with: sudo apt-get install xdotool"
  fi
fi

cat <<MSG

Done. Log out and back in so the new group membership applies, then verify:

  ls -l /dev/uinput            # expect: crw-rw---- root input
  id -nG | tr ' ' '\n' | grep -x input

Then run:  clonecast
MSG
