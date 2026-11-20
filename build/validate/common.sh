# Shared by the validation scripts (sourced, not run).

# harness <engine>: the four TestRoundTrip lines every platform and engine must log.
harness() {
	cat <<LINES
engine=$1 VP8 320x240: 882 kbit/s, min PSNR 50.12 dB
engine=$1 VP8 161x99: 180 kbit/s, min PSNR 50.98 dB
engine=$1 VP9 320x240: 288 kbit/s, min PSNR 51.50 dB
engine=$1 VP9 161x99: 89 kbit/s, min PSNR 48.26 dB
LINES
}

# has <file> <fixed text>: succeeds when the file contains the text.
has() { grep -qF -- "$2" "$1"; }

# hasall <file> <text>...: reports every missing text and fails if any is missing.
hasall() {
	f=$1
	shift
	rc=0
	for want in "$@"; do
		has "$f" "$want" || { echo "  missing: $want" >&2; rc=1; }
	done
	return $rc
}

# harnessall <file> <engine>: hasall for the harness lines.
harnessall() {
	f=$1
	rc=0
	harness "$2" >"$f.want"
	while IFS= read -r line; do
		has "$f" "$line" || { echo "  missing: $line" >&2; rc=1; }
	done <"$f.want"
	return $rc
}
