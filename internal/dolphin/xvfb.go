package dolphin

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Display selection follows the spike's proven range: :91 through :110, a
// number that is free only when neither the X socket nor the lock file is
// present. Checking both matters: a crashed Xvfb leaves its lock, and reusing
// a number whose socket survives would make two bridges share one display —
// the isolation this whole package exists to guarantee.
const (
	displayLowNum   = 91
	displayHighNum  = 110
	xvfbDefaultGeom = "1280x1024x24"
	xvfbDisplayDir  = "/tmp/.X11-unix"
)

// freeDisplay returns the next free private display (":91" form).
func freeDisplay() (string, error) {
	for n := displayLowNum; n <= displayHighNum; n++ {
		if _, err := os.Stat(xvfbDisplayDir + "/X" + strconv.Itoa(n)); err == nil {
			continue // socket present: a live server owns it
		}
		if _, err := os.Stat(fmt.Sprintf("/tmp/.X%d-lock", n)); err == nil {
			continue // lock present: a crashed server owns it
		}
		return ":" + strconv.Itoa(n), nil
	}
	return "", errors.New("dolphin: no free X display in :91-:110")
}

// displayIsUp reports whether the socket for the display exists (the readiness
// check the spike polled with `test -S`).
func displayIsUp(display string) bool {
	n := displayNum(display)
	if n <= 0 {
		return false
	}
	_, err := os.Stat(xvfbDisplayDir + "/X" + strconv.Itoa(n))
	return err == nil
}

func displayNum(display string) int {
	// ":91" or ":91.0"
	s := display
	if len(s) > 0 && s[0] == ':' {
		s = s[1:]
	}
	if i := indexDot(s); i > 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

func indexDot(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}
