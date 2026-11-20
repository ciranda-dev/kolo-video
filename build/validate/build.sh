#!/bin/sh
# Builds every binary of the validation jobs with CGO_ENABLED=0 and writes SHA256SUMS.
#
#   build.sh [-o dir] [windows] [linux] [darwin]     default: all three, into a new temp directory
#
# Prints the directory. The binaries are named <what>-<os>-<arch>[-native].test[.exe]; "-native" is built
# with -tags kolovideo_native (Linux only: the other systems always have the native engine). The h264
# tests are h264-<os>-<arch>.test[.exe] for the six targets and linux/386; their tests read ./testdata,
# so run them from a directory that holds the package's testdata. example-h264-<os>-<arch> is the
# examples/h264 program.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
out=
while getopts o: opt; do
	case $opt in o) out=$OPTARG ;; *) exit 2 ;; esac
done
shift $((OPTIND - 1))
targets=${*:-windows linux darwin}
[ -n "$out" ] || out=$(mktemp -d "${TMPDIR:-/tmp}/kolovideo-validate.XXXXXX")
mkdir -p "$out"
out=$(cd "$out" && pwd)
export CGO_ENABLED=0
cd "$repo"

# test <goos> <goarch> <name> <package> [tags]
test_bin() {
	GOOS=$1 GOARCH=$2 go test -c -tags "${5:-}" -o "$out/$3" "$4"
}
# prog <goos> <goarch> <name> <package> [tags]
prog_bin() {
	GOOS=$1 GOARCH=$2 go build -tags "${5:-}" -o "$out/$3" "$4"
}
static() {
	file "$out/$1" | grep -q 'statically linked' || { echo "build.sh: $1 is not statically linked" >&2; exit 1; }
}

for target in $targets; do
	case $target in
	windows)
		for a in arm64 amd64; do
			test_bin windows $a vpx-windows-$a.test.exe ./vpx
			test_bin windows $a kolovideo-windows-$a.test.exe .
			test_bin windows $a native-windows-$a.test.exe ./vpx/internal/native
			test_bin windows $a h264-windows-$a.test.exe ./h264
			prog_bin windows $a snapshot-windows-$a.exe ./examples/snapshot
			prog_bin windows $a example-h264-windows-$a.exe ./examples/h264
		done
		# no capture backend on windows/386: the unsupported-platform message test runs there
		test_bin windows 386 kolovideo-windows-386.test.exe .
		;;
	linux)
		for a in arm64 amd64; do
			test_bin linux $a vpx-linux-$a.test ./vpx
			test_bin linux $a vpx-linux-$a-native.test ./vpx kolovideo_native
			test_bin linux $a kolovideo-linux-$a.test .
			test_bin linux $a h264-linux-$a.test ./h264
			test_bin linux $a native-linux-$a-native.test ./vpx/internal/native kolovideo_native
			prog_bin linux $a snapshot-linux-$a ./examples/snapshot
			prog_bin linux $a vp8-linux-$a ./examples/vp8
			prog_bin linux $a example-h264-linux-$a ./examples/h264
			for f in vpx-linux-$a.test kolovideo-linux-$a.test h264-linux-$a.test snapshot-linux-$a vp8-linux-$a example-h264-linux-$a; do static "$f"; done
		done
		test_bin linux 386 h264-linux-386.test ./h264
		static h264-linux-386.test
		;;
	darwin)
		for a in arm64 amd64; do
			test_bin darwin $a vpx-darwin-$a.test ./vpx
			test_bin darwin $a kolovideo-darwin-$a.test .
			test_bin darwin $a native-darwin-$a.test ./vpx/internal/native
			test_bin darwin $a h264-darwin-$a.test ./h264
			prog_bin darwin $a snapshot-darwin-$a ./examples/snapshot
			prog_bin darwin $a example-h264-darwin-$a ./examples/h264
		done
		;;
	*) echo "build.sh: unknown target $target" >&2; exit 2 ;;
	esac
done

sums=$(cd "$out" && find . -type f | sed 's|^\./||' | sort | xargs shasum -a 256)
printf '%s\n' "$sums" >"$out/SHA256SUMS"
cat "$out/SHA256SUMS"
echo "built $(wc -l <"$out/SHA256SUMS" | tr -d ' ') binaries in $out"
