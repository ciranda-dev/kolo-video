//go:build linux

package h264

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
	"golang.org/x/sys/unix"
)

func init() {
	platform = &linuxEngine{dir: "/dev", sys: osV4L2, timeout: kolovideo.DefaultStallTimeout}
}

// linuxEngine has no codec. It reports the V4L2 stateful H.264 devices it
// found and does not use, from one scan made on first use.
type linuxEngine struct {
	dir     string
	sys     v4l2Sys
	timeout time.Duration // for all nodes together

	once    sync.Once
	devices []string
}

var errProbeTimeout = errors.New("timed out")

func (e *linuxEngine) name() string { return "" }

func (e *linuxEngine) unavailable() *UnavailableError {
	e.once.Do(func() { e.devices = e.scan() })
	return &UnavailableError{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Reason:  "no H.264 engine on " + runtime.GOOS + "/" + runtime.GOARCH + " in this release",
		Devices: slices.Clone(e.devices),
	}
}

func (e *linuxEngine) caps() Limits {
	err := e.unavailable()
	return Limits{Encode: Support{Err: err}, Decode: Support{Err: err}}
}

func (e *linuxEngine) newEncoder(encParams) (encImpl, error) { return nil, e.unavailable() }
func (e *linuxEngine) newDecoder(decParams) (decImpl, error) { return nil, e.unavailable() }

type nodeResult struct {
	node v4l2Node
	err  error
}

// scan probes the videoN nodes of e.dir (canonical names, as the capture
// listing) in numeric order, all at once under one deadline, and returns one
// line per H.264 device or unreadable node.
func (e *linuxEngine) scan() []string {
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return []string{e.dir + ": not probed (" + errText(err) + ")"}
	}
	var nums []int
	for _, d := range entries {
		if s, ok := strings.CutPrefix(d.Name(), "video"); ok {
			if i, err := strconv.Atoi(s); err == nil && i >= 0 && strconv.Itoa(i) == s {
				nums = append(nums, i)
			}
		}
	}
	slices.Sort(nums)
	paths := make([]string, len(nums))
	results := make([]chan nodeResult, len(nums))
	for i, n := range nums {
		paths[i] = e.dir + "/video" + strconv.Itoa(n)
		results[i] = make(chan nodeResult, 1)
		go func(path string, out chan<- nodeResult) {
			n, err := probeNode(e.sys, path)
			out <- nodeResult{n, err}
		}(paths[i], results[i])
	}
	deadline := time.NewTimer(e.timeout)
	defer deadline.Stop()
	expired := false
	var lines []string
	for i, ch := range results {
		var r nodeResult
		if expired {
			select {
			case r = <-ch:
			default:
				r.err = errProbeTimeout
			}
		} else {
			select {
			case r = <-ch:
			case <-deadline.C:
				expired = true
				r.err = errProbeTimeout
			}
		}
		if line := describe(paths[i], r); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// describe returns the report line for a node, or "" when it is not an
// H.264 device.
func describe(path string, r nodeResult) string {
	if r.err != nil {
		return path + ": not probed (" + errText(r.err) + ")"
	}
	var role string
	switch {
	case r.node.encoder && r.node.decoder:
		role = "encoder and decoder"
	case r.node.encoder:
		role = "encoder"
	case r.node.decoder:
		role = "decoder"
	default:
		return ""
	}
	if r.node.driver != "" {
		role += " (" + r.node.driver + ")"
	}
	return path + ": " + role
}

// errText is the system's text of err, without the path of a failed call.
func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return printable([]byte(err.Error()))
}
