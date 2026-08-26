package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerHealthTracksOperationsIndependently(t *testing.T) {
	manager := &Manager{statePath: filepath.Join(t.TempDir(), "stanza")}
	if err := manager.setRepositoryState("healthy"); err != nil {
		t.Fatal(err)
	}
	if !manager.Healthy() {
		t.Fatal("Healthy() = false before a scheduled backup has run")
	}
	if err := manager.setState("full", "failed"); err != nil {
		t.Fatal(err)
	}
	if err := manager.setRepositoryState("healthy"); err != nil {
		t.Fatal(err)
	}
	if manager.Healthy() {
		t.Fatal("Healthy() = true after a full backup failed and repository check recovered")
	}
	if err := manager.setState("full", "healthy"); err != nil {
		t.Fatal(err)
	}
	if err := manager.setState("differential", "failed"); err != nil {
		t.Fatal(err)
	}
	if manager.Healthy() {
		t.Fatal("Healthy() = true after a differential backup failed")
	}
	if err := manager.setState("differential", "healthy"); err != nil {
		t.Fatal(err)
	}
	if !manager.Healthy() {
		t.Fatal("Healthy() = false after all failed operations recovered")
	}
}

func TestManagerHealthWaitsForRequiredInitialBackup(t *testing.T) {
	manager := &Manager{InitialBackup: true, statePath: filepath.Join(t.TempDir(), "stanza")}
	if err := manager.SetStarting(); err != nil {
		t.Fatal(err)
	}
	if err := manager.setRepositoryState("healthy"); err != nil {
		t.Fatal(err)
	}
	if manager.Healthy() {
		t.Fatal("Healthy() = true before the required initial backup completed")
	}
	if err := manager.setInitialState("healthy"); err != nil {
		t.Fatal(err)
	}
	if !manager.Healthy() {
		t.Fatal("Healthy() = false after the required initial backup completed")
	}
}

func TestAliasS3HostReplacesPreviousAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := aliasS3Host(path, "fd00::1"); err != nil {
		t.Fatal(err)
	}
	if err := aliasS3Host(path, "fd00::2"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "127.0.0.1 localhost\nfd00::2 " + s3HostAlias + "\n"
	if string(contents) != want {
		t.Fatalf("hosts file = %q, want %q", contents, want)
	}
}
