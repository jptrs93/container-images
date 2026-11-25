package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var forwardedSignals = []os.Signal{
	syscall.SIGHUP,
	syscall.SIGINT,
	syscall.SIGQUIT,
	syscall.SIGTERM,
	syscall.SIGUSR1,
	syscall.SIGUSR2,
	syscall.SIGWINCH,
}

// runInit is the whole of the supervisor when it is PID 1: it starts the
// command, forwards signals to it, reaps every process the kernel reparents to
// PID 1, and returns the command's exit code.
//
// The supervisor proper must not be the one reaping. A wait on any child steals
// the exit status of the commands it runs through os/exec, which then fail with
// "no child processes" although they succeeded. This half therefore runs no
// commands of its own and never waits on the child through os.Process.
func runInit(path string, args []string) int {
	signals := make(chan os.Signal, 32)
	signal.Notify(signals, forwardedSignals...)
	child, err := os.StartProcess(path, args, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		slog.Error("start supervisor: " + err.Error())
		return 1
	}
	go func() {
		for received := range signals {
			_ = child.Signal(received)
		}
	}()
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			slog.Error("wait for supervisor: " + err.Error())
			return 1
		}
		if pid != child.Pid {
			continue
		}
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
		return status.ExitStatus()
	}
}
