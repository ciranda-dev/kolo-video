#!/bin/sh
# V4L2 streaming code against the kernel's vivid virtual capture driver, in a Linux VM.
#
#   KOLO_VM=<helper> linux-v4l2.sh
#
# KOLO_VM is required: an executable that understands `put <file>` (copy a file to the VM's home
# directory) and `ssh <command>` (run a command in the VM as a user with sudo and print its output).
# The VM must run a Debian or Ubuntu kernel that has the vivid module and boot as arm64 (or build for
# its architecture with KOLO_VM_GOARCH). No camera exists in the VM: vivid makes test pictures.
#
# Prints the fake-layer test result on the VM kernel, then TestV4L2Vivid. The last line is PASS or FAIL.
set -eu

vm=${KOLO_VM:-}
[ -n "$vm" ] && [ -x "$vm" ] || { echo "linux-v4l2.sh: set KOLO_VM to the VM helper (see the header)" >&2; exit 2; }
arch=${KOLO_VM_GOARCH:-arm64}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$tmp/kolovideo.test" .)
"$vm" put "$tmp/kolovideo.test"
"$vm" ssh 'uname -srm; sudo modprobe vivid n_devs=1 node_types=0x1; grep -H . /sys/class/video4linux/*/name'
fail=0
echo "=== the fake-layer tests on the VM kernel"
"$vm" ssh './kolovideo.test -test.run V4L2 -test.count=5' || fail=1
echo "=== vivid"
"$vm" ssh 'sudo KOLO_VIDEO_V4L2_TEST=1 ./kolovideo.test -test.run "^TestV4L2Vivid$" -test.v -test.count=1' | tee "$tmp/vivid.log" || fail=1
grep -q -- '--- PASS: TestV4L2Vivid' "$tmp/vivid.log" || fail=1
if [ $fail -eq 0 ]; then echo PASS; else echo FAIL; exit 1; fi
