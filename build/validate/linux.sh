#!/bin/sh
# Linux validation in Docker, including the hostile cases of the native loader.
#
#   linux.sh [arch ...]      arch is arm64 or amd64; default both
#
# Builds the Linux binaries with CGO_ENABLED=0 (build.sh), then runs them in containers with no
# network and no devices. An amd64 run on an arm64 host is emulated: it is a functional check only,
# it uses -test.short, and the noexec cases are skipped because the emulator does not enforce
# noexec mappings. Needs the images below (pulled once). Run one job at a time.
#
# Prints "ok" for each case and PASS at the end; any failed case prints its output and the script
# exits 1.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
. "$here/common.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/kolovideo-linux.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
archs=${*:-arm64 amd64}
host=$(uname -m)
case $host in aarch64) host=arm64 ;; x86_64) host=amd64 ;; esac

"$here/build.sh" -o "$tmp/bin" linux >/dev/null
cat >"$tmp/nomemfd.json" <<'JSON'
{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["memfd_create"],"action":"SCMP_ACT_ERRNO","errnoRet":1}]}
JSON

fail=0
# want holds the text the output must contain, one item per line; job clears it.
want=

# job <label> <pass|nostart> <image> <docker flags...> -- <program and arguments>
# The program is the path inside the container; the binary is mounted at /t.
job() {
	label=$1 mode=$2 image=$3
	shift 3
	flags=
	while [ "$1" != -- ]; do flags="$flags $1"; shift; done
	shift
	entry=$1
	shift
	rc=0
	# shellcheck disable=SC2086
	docker run --rm --network none --platform "linux/$arch" -v "$bin:/t:ro" $flags --entrypoint "$entry" "$image" "$@" >"$tmp/out" 2>&1 </dev/null || rc=$?
	ok=0
	case $mode in
	pass)
		[ $rc -eq 0 ] || ok=1
		printf '%s\n' "$want" | while IFS= read -r line; do
			[ -z "$line" ] || has "$tmp/out" "$line" || { echo "  missing: $line" >&2; exit 1; }
		done || ok=1
		grep -q -- '--- FAIL' "$tmp/out" && ok=1
		;;
	nostart)
		[ $rc -ne 0 ] || ok=1
		grep -qiE 'not found|no such file' "$tmp/out" || ok=1
		grep -q 'PASS' "$tmp/out" && ok=1
		;;
	esac
	want=
	if [ $ok -eq 0 ]; then
		echo "ok   $label"
	else
		echo "FAIL $label (exit $rc)"
		fail=1
		tail -25 "$tmp/out" | sed 's/^/  | /'
	fi
}

for arch in $archs; do
	echo "== linux/$arch"
	b=$tmp/bin
	short=
	[ "$arch" = "$host" ] || short=-test.short
	noexec=1
	[ "$arch" = "$host" ] || noexec=0
	file "$b/vpx-linux-$arch.test" | grep -q 'statically linked'
	file "$b/vpx-linux-$arch-native.test" | grep -q 'dynamically linked' && echo "     vpx -tags kolovideo_native is dynamically linked, as documented"

	# Without the tag: static, portable engine, starts everywhere.
	for image in debian:bookworm-slim alpine:3 gcr.io/distroless/static-debian12; do
		bin=$b/vpx-linux-$arch.test
		want=$(harness portable; echo PASS)
		job "vpx without the tag on $image: starts, engine=portable" pass "$image" -- /t -test.v -test.count=1 -test.timeout 30m $short
	done
	bin=$b/kolovideo-linux-$arch.test
	for image in debian:bookworm-slim alpine:3; do
		want=PASS
		job "root tests on $image (V4L2 fakes, no /dev/video*)" pass "$image" -- /t -test.v -test.count=1 -test.timeout 30m $short
	done
	bin=$b/snapshot-linux-$arch
	job "examples/snapshot -list on alpine:3 (no camera, exit 0)" pass alpine:3 -- /t -list

	# h264: Linux has no engine. The tests read ./testdata, so the package directory is mounted
	# and used as the working directory. The program must print the typed absence and use VP8.
	for image in debian:bookworm-slim alpine:3; do
		bin=$b/h264-linux-$arch.test
		want=PASS
		job "h264 tests on $image (absence, parsers, fuzz seeds; no devices)" pass "$image" -v "$repo/h264:/pkg:ro" -w /pkg -- /t -test.v -test.count=1 -test.timeout 30m $short
		bin=$b/example-h264-linux-$arch
		want=$(printf 'H.264 is not available, using VP8: kolovideo/h264: no H.264 engine on linux/%s in this release; V4L2 stateful H.264 devices: none\nVP8 through libvpx\nworst PSNR' "$arch")
		job "examples/h264 on $image: typed absence, VP8 fallback" pass "$image" -- /t
	done

	# With the tag: native engine on glibc hosts, does not start without glibc.
	bin=$b/vpx-linux-$arch-native.test
	for image in debian:bookworm-slim ubuntu:24.04; do
		want=$(harness native; harness portable; echo PASS)
		job "vpx with kolovideo_native on $image: engine=native and portable" pass "$image" -- /t -test.v -test.count=1 -test.timeout 30m $short
	done
	for image in alpine:3 gcr.io/distroless/static-debian12; do
		job "vpx with kolovideo_native on $image: does not start (documented limit)" nostart "$image" -- /t -test.v
	done
	want='--- PASS: TestHostileLinux'
	job "vpx with kolovideo_native on distroless/base: engine=native" pass gcr.io/distroless/base-debian12 -e KOLOVIDEO_HOSTILE=native -- /t -test.v -test.run '^TestHostileLinux$' -test.count=1 -test.timeout 30m
	want='--- PASS: TestHostileLinux'
	job "old glibc 2.31 (bullseye), embedded library: engine=native" pass debian:bullseye-slim -e KOLOVIDEO_HOSTILE=native -- /t -test.v -test.run '^TestHostileLinux$' -test.count=1 -test.timeout 30m
	want=$(printf -- '--- PASS: TestHostileLinux\nGLIBC_2.34')
	job "old glibc 2.31, library that needs 2.34: portable fallback, typed error naming GLIBC_2.34" pass debian:bullseye-slim -e KOLOVIDEO_HOSTILE=portable -e KOLOVIDEO_HOSTILE_LIB=glibc -e KOLOVIDEO_HOSTILE_ERR=GLIBC_2.34 -- /t -test.v -test.run '^TestHostileLinux$' -test.count=1 -test.timeout 30m

	# Loader scenarios: which source the loader used.
	bin=$b/native-linux-$arch-native.test
	scenario() { # scenario <label> <expect> <docker flags...>
		label=$1 expect=$2
		shift 2
		want=$(printf -- '--- PASS: TestScenario\nRESULT %s' "$expect")
		job "loader $label: $expect" pass debian:bookworm-slim "$@" -e KV_T2_EXPECT="$expect" -e KV_T2_FIXTURE=embedded -e KV_T2_CACHE=/tmp/kvcache -- /t -test.v -test.run '^TestScenario$' -test.count=1 -test.timeout 30m
	}
	scenario "default" memfd
	scenario "read-only root, noexec /tmp, memfd available" memfd --read-only --tmpfs /tmp:rw,noexec
	scenario "seccomp denies memfd_create" cache --security-opt "seccomp=$tmp/nomemfd.json"
	want=$(printf -- '--- PASS: TestScenario\nRESULT cache')
	job "loader with no /proc (umount, privileged): cache" pass debian:bookworm-slim --privileged -e KV_T2_EXPECT=cache -e KV_T2_FIXTURE=embedded -e KV_T2_CACHE=/tmp/kvcache -- /bin/sh -c 'umount /proc && exec /t -test.v -test.run "^TestScenario$" -test.count=1'
	if [ $noexec -eq 1 ]; then
		scenario "seccomp denies memfd_create and /tmp is noexec" fail --security-opt "seccomp=$tmp/nomemfd.json" --tmpfs /tmp:rw,noexec
		want=$(printf -- '--- PASS: TestScenario\nRESULT fail')
		job "loader with no /proc and noexec /tmp: fail" pass debian:bookworm-slim --privileged --tmpfs /tmp:rw,noexec -e KV_T2_EXPECT=fail -e KV_T2_FIXTURE=embedded -e KV_T2_CACHE=/tmp/kvcache -- /bin/sh -c 'umount /proc && exec /t -test.v -test.run "^TestScenario$" -test.count=1'
		bin=$b/vpx-linux-$arch-native.test
		want='--- PASS: TestHostileLinux'
		job "vpx: seccomp without memfd_create and noexec /tmp: portable fallback, typed error" pass debian:bookworm-slim --security-opt "seccomp=$tmp/nomemfd.json" --tmpfs /tmp:rw,noexec -e KOLOVIDEO_HOSTILE=portable -e KOLOVIDEO_HOSTILE_ERR=memfd_create -- /t -test.v -test.run '^TestHostileLinux$' -test.count=1 -test.timeout 30m 
	else
		echo "skip loader and vpx noexec cases on emulated $arch"
	fi
done

# linux/386 has no codec either; the h264 tests ran there in an i386 image (emulated on a 64-bit host).
echo "== linux/386"
arch=386
bin=$tmp/bin/h264-linux-386.test
want=PASS
job "h264 tests on i386/alpine:3 (32-bit absence and parsers)" pass i386/alpine:3 -v "$repo/h264:/pkg:ro" -w /pkg -- /t -test.v -test.count=1 -test.timeout 30m -test.short

if [ $fail -eq 0 ]; then echo PASS; else echo FAIL; exit 1; fi
