package backup

import (
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
