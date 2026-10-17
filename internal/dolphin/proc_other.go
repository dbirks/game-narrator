//go:build !unix

package dolphin

import (
	"errors"
	"io"
)

// Non-unix stubs: the production bridge needs Xvfb, process-group signals
// and mkfifo, none of which exist on other platforms. The unit tests run
// here too, because they use the fake seams and never call these.

func realStartXvfb(bin string) func(display, geometry string, logf io.Writer) (pid int, stop func() error, err error) {
	return func(display, geometry string, logf io.Writer) (pid int, stop func() error, err error) {
		return 0, nil, errors.New("dolphin: Xvfb launch is unix-only (dev host tool); wire a fake StartXvfb seam for tests")
	}
}

func realStartDolphin(bin string) func(display string, args []string, logf io.Writer) (pid int, stop func() error, err error) {
	return func(display string, args []string, logf io.Writer) (pid int, stop func() error, err error) {
		return 0, nil, errors.New("dolphin: emulator launch is unix-only (dev host tool); wire a fake StartDolphin seam for tests")
	}
}

func procAlive(pid int) bool { return false }

func mkFIFO(path string, mode uint32) error {
	return errors.New("dolphin: mkfifo is unix-only")
}
