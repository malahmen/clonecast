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
sudo usermod -aG input "$USER"

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

cat <<MSG

Done. Log out and back in so the new group membership applies, then verify:

  ls -l /dev/uinput            # expect: crw-rw---- root input
  id -nG | tr ' ' '\n' | grep -x input

Then run:  clonecast
MSG
