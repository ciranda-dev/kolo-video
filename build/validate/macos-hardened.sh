#!/bin/sh
# Hardened-runtime check on macOS. Builds the darwin test binaries, signs them ad hoc with
# `codesign -s - -f -o runtime`, runs them and prints PASS only when every check below holds.
#
#   macos-hardened.sh [arch ...]     arch is arm64 or amd64 (amd64 runs under Rosetta); default: both when possible
#
# vpx, no entitlement: Load falls back to the portable engine, the native error names
#   "different Team IDs", the portable round trip passes.
# vpx, disable-library-validation: Load picks the native engine, TestRoundTrip passes on both engines.
# root, no entitlement: the AVFoundation listing (no camera is opened) and the synthetic delegate tests pass.
# h264, as built and hardened with no entitlement: VideoToolbox round trips, the software path, damaged input,
#   the Windows streams in h264/testdata; none of them may be skipped. The Capabilities lines are printed.
# No camera is opened and RequestAccess is never called: KOLO_VIDEO_HW_TEST is unset and only the
# listing and fake-device tests run.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
. "$here/common.sh"
[ "$(uname -s)" = Darwin ] || { echo "macos-hardened.sh: needs macOS" >&2; exit 2; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/kolovideo-hardened.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
if [ $# -gt 0 ]; then
	archs=$*
elif [ "$(uname -m)" != arm64 ]; then
	archs=amd64
elif arch -x86_64 /usr/bin/true 2>/dev/null; then
	archs="arm64 amd64"
else
	archs=arm64
fi

"$here/build.sh" -o "$tmp/bin" darwin >/dev/null
cat >"$tmp/entitlements.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>com.apple.security.cs.disable-library-validation</key><true/></dict></plist>
PLIST

fail=0
unset KOLO_VIDEO_HW_TEST KOLO_VIDEO_LEAK_LISTINGS || true
# The runs get a temp HOME; the tests that call `go list` keep the caller's module and build caches,
# so the script works offline whenever the build above did.
gomodcache=$(go env GOMODCACHE)
gocache=$(go env GOCACHE)

# run <label> <arch> <env assignments...> -- <binary> <args...>: output in $tmp/out, status in $rc
run() {
	label=$1 a=$2
	shift 2
	envs=
	while [ "$1" != -- ]; do envs="$envs $1"; shift; done
	shift
	archname=$a
	[ "$a" != amd64 ] || archname=x86_64
	mkdir -p "$tmp/work/$a/tmp"
	rc=0
	# shellcheck disable=SC2086
	(cd "${wd:-.}" && env $envs TMPDIR="$tmp/work/$a/tmp" HOME="$tmp/work/$a" GOMODCACHE="$gomodcache" GOCACHE="$gocache" arch -"$archname" "$@" -test.count=1 -test.v -test.timeout "${timeout:-5m}") >"$tmp/out" 2>&1 || rc=$?
	cp "$tmp/out" "$tmp/out.$label.$a"
}

verdict() { # verdict <label> <ok 0|1>
	if [ "$2" -eq 0 ]; then echo "ok   $1"; else echo "FAIL $1"; fail=1; sed 's/^/  | /' "$tmp/out" | tail -25; fi
}

for a in $archs; do
	echo "== darwin/$a"
	plain=$tmp/hardened-$a
	ent=$tmp/entitled-$a
	for bin in vpx kolovideo; do
		cp "$tmp/bin/$bin-darwin-$a.test" "$plain-$bin"
		codesign -s - -f -o runtime -i "dev.example.kolovideo.$bin" "$plain-$bin" 2>/dev/null
	done
	cp "$tmp/bin/vpx-darwin-$a.test" "$ent-vpx"
	codesign -s - -f -o runtime --entitlements "$tmp/entitlements.plist" -i dev.example.kolovideo.vpx "$ent-vpx" 2>/dev/null
	codesign --verify "$plain-vpx"
	codesign -d --entitlements - "$ent-vpx" 2>/dev/null | grep -q disable-library-validation

	run vpx-plain-load "$a" KOLOVIDEO_VPX_CHILD=1 KOLOVIDEO_VPX_CHILD_CACHE="$tmp/work/$a/cache1" -- "$plain-vpx" -test.run '^TestHardenedChild$'
	ok=$rc
	hasall "$tmp/out" "KVRESULT " '"Engine":"portable"' "different Team IDs" "disable-library-validation" "--- PASS: TestHardenedChild" || ok=1
	verdict "vpx without the entitlement: engine=portable, native error names the Team IDs" $ok
	grep -h '^KVRESULT' "$tmp/out" | cut -c1-400 | sed 's/^/     /' || true

	run vpx-plain-portable "$a" -- "$plain-vpx" -test.run '^TestRoundTrip$/^portable$'
	ok=$rc
	harnessall "$tmp/out" portable || ok=1
	verdict "vpx without the entitlement: portable round trip, harness lines" $ok

	run vpx-entitled-load "$a" KOLOVIDEO_VPX_CHILD=1 KOLOVIDEO_VPX_CHILD_CACHE="$tmp/work/$a/cache2" -- "$ent-vpx" -test.run '^TestHardenedChild$'
	ok=$rc
	hasall "$tmp/out" "KVRESULT " '"Engine":"native"' '"Source":"cache"' "--- PASS: TestHardenedChild" || ok=1
	verdict "vpx with disable-library-validation: engine=native from the cache" $ok

	run vpx-entitled-roundtrip "$a" -- "$ent-vpx" -test.run '^TestRoundTrip$'
	ok=$rc
	harnessall "$tmp/out" native || ok=1
	harnessall "$tmp/out" portable || ok=1
	verdict "vpx with disable-library-validation: both engines, harness lines" $ok

	run root "$a" -- "$plain-kolovideo"
	ok=$rc
	hasall "$tmp/out" "--- PASS: TestRealListing" "--- PASS: TestAVFListing" "--- PASS: TestDelegateNV12" "--- PASS: TestDelegateBGRA" "--- PASS: TestRealSessionObjects" || ok=1
	if grep -q -- '--- FAIL' "$tmp/out"; then ok=1; fi
	verdict "root without the entitlement: listing and synthetic delegate tests (no camera opened)" $ok

	# The h264 tests read ./testdata, so they run in the package directory. Frames are generated.
	cp "$tmp/bin/h264-darwin-$a.test" "$tmp/h264-plain-$a"
	cp "$tmp/bin/h264-darwin-$a.test" "$tmp/h264-hardened-$a"
	codesign -s - -f -o runtime -i dev.example.kolovideo.h264 "$tmp/h264-hardened-$a" 2>/dev/null
	for kind in plain hardened; do
		wd=$repo/h264
		timeout=20m
		run "h264-$kind" "$a" -- "$tmp/h264-$kind-$a" -test.skip '^TestVTPoolChoice$'
		wd=
		timeout=
		ok=$rc
		hasall "$tmp/out" "--- PASS: TestVTRoundTrip" "--- PASS: TestVTSoftwarePath" "--- PASS: TestVTCorrupt" "--- PASS: TestVTEngineCorrupt" "--- PASS: TestVTInterop" "--- PASS: TestVTCapabilitiesReal" || ok=1
		if grep -q -- '--- FAIL' "$tmp/out" || grep -q -- '--- SKIP: TestVT\(RoundTrip\|SoftwarePath\|Corrupt\|EngineCorrupt\|Interop\|CapabilitiesReal\)' "$tmp/out"; then ok=1; fi
		verdict "h264 $kind: VideoToolbox round trips, software path, damaged input, Windows streams" $ok
		grep -h -A1 'encode {Available' "$tmp/out" | cut -c1-300 | sed 's/^/     /' || true
	done
done

if [ $fail -eq 0 ]; then echo PASS; else echo FAIL; exit 1; fi
