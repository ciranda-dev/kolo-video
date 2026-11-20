#!/bin/sh
# Windows validation on a Windows 11 ARM64 machine or VM (arm64 native, amd64 under emulation).
#
#   windows.sh [arch ...]     arch is arm64 or amd64; default both
#
# Builds the Windows binaries with CGO_ENABLED=0 (build.sh) and runs them one at a time through
# the runner script, which copies a binary to the VM, runs it there and prints its output.
#   KOLO_WINRUN   required: the runner, an executable that takes a Windows binary and its arguments,
#                 copies the binary to the Windows machine, runs it there, prints its output and
#                 exits with its status. There is no default. For the h264 tests it is also given a
#                 directory (the test binary and its testdata folder) with EXE naming the program in it,
#                 and WINENV="NAME=value" naming an environment variable to set for the run.
# The amd64 run is under emulation: it uses -test.short and is a functional check only. No camera is
# opened: the root tests enumerate and read a generated H.264 file with the Media Foundation source reader.
# The h264 tests run with KOLOVIDEO_H264_REQUIRE=1, so a missing H.264 transform fails instead of skipping.
# Run one job at a time.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
. "$here/common.sh"
winrun=${KOLO_WINRUN:-}
[ -n "$winrun" ] || { echo "windows.sh: set KOLO_WINRUN to the runner (see the header)" >&2; exit 2; }
[ -x "$winrun" ] || { echo "windows.sh: no runner at $winrun" >&2; exit 2; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/kolovideo-windows.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
archs=${*:-arm64 amd64}

"$here/build.sh" -o "$tmp/bin" windows >/dev/null
fail=0
want=

# job <label> <binary> <args...>: runs the binary on the VM; the output must contain $want (one item per line)
job() {
	label=$1 exe=$2
	shift 2
	rc=0
	case $exe in /*) ;; *) exe=$tmp/bin/$exe ;; esac
	"$winrun" "$exe" "$@" >"$tmp/out" 2>&1 || rc=$?
	tr -d '\r' <"$tmp/out" >"$tmp/out.lf"
	ok=0
	[ $rc -eq 0 ] || ok=1
	printf '%s\n' "$want" | while IFS= read -r line; do
		[ -z "$line" ] || has "$tmp/out.lf" "$line" || { echo "  missing: $line" >&2; exit 1; }
	done || ok=1
	grep -q -- '--- FAIL' "$tmp/out.lf" && ok=1
	want=
	if [ $ok -eq 0 ]; then
		echo "ok   $label"
	else
		echo "FAIL $label (exit $rc)"
		fail=1
		tail -25 "$tmp/out.lf" | sed 's/^/  | /'
	fi
}

for arch in $archs; do
	echo "== windows/$arch"
	short=
	[ "$arch" = arm64 ] || short=-test.short
	want=$(harness native; harness portable; echo PASS)
	job "vpx on both engines: harness lines" vpx-windows-"$arch".test.exe -test.v -test.count=1 -test.timeout 30m $short
	want=$(printf -- '--- PASS: TestMFEnumerate\n--- PASS: TestMFReaderFile\nPASS')
	job "root: MFStartup and enumeration, source reader on a generated file" kolovideo-windows-"$arch".test.exe -test.v -test.count=1 -test.timeout 30m
	if grep -q -- '--- SKIP: TestMF\(Enumerate\|ReaderFile\)' "$tmp/out.lf"; then echo "FAIL a Media Foundation test was skipped"; fail=1; fi
	want=PASS
	job "native loader: DLL extraction, verification, load" native-windows-"$arch".test.exe -test.v -test.count=1 -test.timeout 30m
	want=
	job "examples/snapshot -list (no camera, exit 0)" snapshot-windows-"$arch".exe -list

	# h264: Media Foundation transforms, generated frames. The tests read ./testdata, so the binary
	# runs from a directory that holds it. Emulation skips the long runs (-test.short).
	stage=$tmp/h264-$arch
	mkdir -p "$stage"
	cp "$tmp/bin/h264-windows-$arch.test.exe" "$stage/"
	cp -R "$repo/h264/testdata" "$stage/testdata"
	want=$(printf -- '--- PASS: TestMFTCapabilitiesReal\n--- PASS: TestMFTProfiles\n--- PASS: TestMFTRoundTrip\n--- PASS: TestMFTOnePerInput\n--- PASS: TestMFTInterop\nPASS')
	if [ "$arch" = arm64 ]; then
		want=$(printf '%s\n--- PASS: TestMFTPaddedLayout\n--- PASS: TestMFTLevel6Refused\n--- PASS: TestMFTCorrupt\n--- PASS: TestMFTOpenClose\n--- PASS: TestMFTBitrateChange' "$want")
	fi
	export EXE=h264-windows-$arch.test.exe WINENV=KOLOVIDEO_H264_REQUIRE=1
	job "h264: Media Foundation round trips, one picture per input, interop streams" "$stage" -test.v -test.count=1 -test.timeout 30m $short
	unset EXE WINENV
	want=$(printf 'H.264 through Media Foundation\ncodec string avc3.42c0')
	job "examples/h264: H.264 through Media Foundation" example-h264-windows-"$arch".exe
done

case " $archs " in *" arm64 "*)
	want=$(printf -- '--- PASS: TestUnsupportedPlatformMessage\nPASS')
	echo "== windows/386 (no capture backend; x86 emulation on the arm64 VM)"
	job "root: ErrUnsupported names windows/386" kolovideo-windows-386.test.exe -test.v -test.count=1 -test.timeout 5m -test.run '^TestUnsupportedPlatformMessage$'
	;;
esac

if [ $fail -eq 0 ]; then echo PASS; else echo FAIL; exit 1; fi
