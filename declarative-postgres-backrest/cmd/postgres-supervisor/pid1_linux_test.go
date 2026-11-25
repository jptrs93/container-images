package main

import (
	"path/filepath"
	"syscall"
	"testing"
)

const prSetChildSubreaper = 36

// Orphans are reparented to PID 1 or to the nearest subreaper, so the helper
// becomes one to receive them the way PID 1 does in a container.
func init() {
	prepareInitHelper = func() {
		if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
			panic("set child subreaper: " + errno.Error())
		}
	}
}

func TestRunInitReapsOrphans(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	script := `( sleep 0.2 & echo $! > '` + pidFile + `' )
orphan="$(cat '` + pidFile + `')"
attempts=0
while [ -e "/proc/$orphan" ]; do
	attempts=$((attempts + 1))
	[ "$attempts" -gt 100 ] && exit 9
	sleep 0.05
done
exit 0`
	if got := helperExitCode(t, initHelper(t, script).Run()); got != 0 {
		t.Fatalf("exit code = %d: the orphaned process was left defunct", got)
	}
}
