package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

const (
	initHelperEnv = "POSTGRES_SUPERVISOR_TEST_INIT"
	initScriptEnv = "POSTGRES_SUPERVISOR_TEST_SCRIPT"
)

var prepareInitHelper = func() {}

// runInit waits on every child of its process, so the tests run it in a copy
// of the test binary rather than in the test process itself.
func TestMain(m *testing.M) {
	if os.Getenv(initHelperEnv) == "1" {
		prepareInitHelper()
		os.Exit(runInit("/bin/sh", []string{"sh", "-c", os.Getenv(initScriptEnv)}))
	}
	os.Exit(m.Run())
}

func initHelper(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(), initHelperEnv+"=1", initScriptEnv+"="+script)
	command.Stderr = os.Stderr
	return command
}

func helperExitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run init helper: %v", err)
	}
	return exitError.ExitCode()
}

func TestRunInitReturnsCommandExitCode(t *testing.T) {
	for script, want := range map[string]int{
		"exit 0":        0,
		"exit 7":        7,
		"kill -KILL $$": 128 + int(syscall.SIGKILL),
	} {
		if got := helperExitCode(t, initHelper(t, script).Run()); got != want {
			t.Errorf("%q: exit code = %d, want %d", script, got, want)
		}
	}
}

func TestRunInitForwardsSignals(t *testing.T) {
	for _, forwarded := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1} {
		command := initHelper(t, "trap 'exit 42' TERM INT HUP QUIT USR1; echo ready; while :; do sleep 0.05; done")
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("%v: wait for command: %q, %v", forwarded, line, err)
		}
		if err := command.Process.Signal(forwarded); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case err := <-done:
			if got := helperExitCode(t, err); got != 42 {
				t.Errorf("%v: exit code = %d, want 42 from the command's trap", forwarded, got)
			}
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			t.Fatalf("%v was not forwarded to the command", forwarded)
		}
	}
}
