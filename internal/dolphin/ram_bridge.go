//go:build unix

package dolphin

// Mem1 implements Mem1Provider on the live bridge: it opens (once, cached
// per process) the read-only MEM1 mapping of the Dolphin this bridge
// launched. If the mapping cannot be opened (Dolphin not running, or
// /proc/<pid>/mem denied) the typed ErrMem1* propagates so callers report
// NOT RUN.
func (b *bridge) Mem1() (*Mem1, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mem1 != nil && b.mem1PID == b.st.DolphinPID {
		return b.mem1, nil
	}
	if b.mem1 != nil {
		_ = b.mem1.Close()
		b.mem1 = nil
	}
	if b.st.DolphinPID == 0 {
		return nil, ErrMem1NoProcess
	}
	m, err := OpenMem1(b.st.DolphinPID)
	if err != nil {
		return nil, err
	}
	b.mem1 = m
	b.mem1PID = b.st.DolphinPID
	return m, nil
}
