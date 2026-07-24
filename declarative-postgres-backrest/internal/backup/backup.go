package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jptrs93/container-images/declarative-postgres-backrest/internal/config"
	"github.com/jptrs93/container-images/declarative-postgres-backrest/internal/status"
	"github.com/robfig/cron/v3"
)

const (
	configPath = "/etc/pgbackrest/pgbackrest.conf"
	stateRoot  = "/var/lib/postgresql/pgbackrest-state"
	spoolPath  = "/var/lib/postgresql/pgbackrest-spool"
)

type Manager struct {
	Stanza          string
	InitialBackup   bool
	FullSchedule    string
	DiffSchedule    string
	CheckSchedule   string
	Timezone        string
	commandMu       sync.Mutex
	stateMu         sync.RWMutex
	archiveTimeout  string
	statePath       string
	repositoryState string
	fullState       string
	diffState       string
	initialState    string
}

func New(pgData, postgresUser string, connection config.ConnectionOptions, options config.PGBackRestOptions) (*Manager, error) {
	if postgresUser == "" {
		postgresUser = "postgres"
	}
	if !filepath.IsAbs(pgData) || strings.ContainsAny(pgData, "\r\n") || strings.ContainsAny(postgresUser, "\r\n") {
		return nil, fmt.Errorf("PostgreSQL data path must be absolute, and the path and user must each be a single line")
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0750); err != nil {
		return nil, err
	}
	if err := os.Chown(filepath.Dir(configPath), 999, 999); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spoolPath, 0750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll("/var/log/pgbackrest", 0750); err != nil {
		return nil, err
	}

	verify := "n"
	host := options.S3Host
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.To4() == nil {
		host = "[" + strings.Trim(host, "[]") + "]"
	}
	endpoint := "http://" + host
	if options.S3VerifyTLS {
		verify = "y"
		endpoint = "https://" + host
	}
	lines := []string{
		"[global]",
		"repo1-type=s3",
		"repo1-path=/",
		"repo1-s3-endpoint=" + endpoint,
		"repo1-s3-port=" + strconv.Itoa(options.S3Port),
		"repo1-s3-bucket=" + options.S3Bucket,
		"repo1-s3-region=" + options.S3Region,
		"repo1-s3-key=" + options.S3AccessKey,
		"repo1-s3-key-secret=" + options.S3SecretKey,
		"repo1-s3-uri-style=" + options.S3URIStyle,
		"repo1-storage-verify-tls=" + verify,
		"repo1-retention-full=" + strconv.Itoa(options.RetentionFull),
		"repo1-retention-archive=" + strconv.Itoa(options.RetentionArchive),
		"archive-async=y",
		"archive-push-queue-max=" + options.ArchivePushQueueMax,
		"archive-timeout=" + strconv.Itoa(options.ArchiveTimeoutSeconds),
		"process-max=" + strconv.Itoa(options.ProcessMax),
		"compress-type=zst",
		"repo1-bundle=y",
		"repo1-block=y",
		"start-fast=y",
		"log-level-console=info",
		"log-level-file=detail",
		"spool-path=" + spoolPath,
	}
	if options.RepositoryCipherPass != "" {
		lines = append(lines, "repo1-cipher-type=aes-256-cbc", "repo1-cipher-pass="+options.RepositoryCipherPass)
	}
	lines = append(lines, "", "["+options.Stanza+"]", "pg1-path="+pgData, "pg1-port="+connection.Port, "pg1-socket-path="+connection.SocketDirectory, "pg1-user="+postgresUser)
	if err := os.WriteFile(configPath, []byte(strings.Join(lines, "\n")+"\n"), 0640); err != nil {
		return nil, err
	}
	if err := os.Chown(configPath, 999, 999); err != nil {
		return nil, err
	}
	if err := os.Chown(spoolPath, 999, 999); err != nil {
		return nil, err
	}
	if err := os.Chown("/var/log/pgbackrest", 999, 999); err != nil {
		return nil, err
	}

	manager := &Manager{
		Stanza:         options.Stanza,
		InitialBackup:  options.InitialBackup,
		FullSchedule:   options.FullSchedule,
		DiffSchedule:   options.DiffSchedule,
		CheckSchedule:  options.CheckSchedule,
		Timezone:       options.Timezone,
		archiveTimeout: strconv.Itoa(options.ArchiveTimeoutSeconds),
		statePath:      filepath.Join(stateRoot, options.Stanza),
	}
	manager.fullState = status.Read(manager.fullStatePath())
	manager.diffState = status.Read(manager.diffStatePath())
	return manager, nil
}

func (manager *Manager) ArchiveCommand() string {
	return "pgbackrest --stanza=" + manager.Stanza + " archive-push '%p'"
}

func (manager *Manager) ArchiveTimeout() string {
	return manager.archiveTimeout
}

func (manager *Manager) SetStarting() error {
	if err := manager.setRepositoryState("starting"); err != nil {
		return err
	}
	if manager.InitialBackup {
		return manager.setInitialState("starting")
	}
	return nil
}

func (manager *Manager) Healthy() bool {
	manager.stateMu.RLock()
	defer manager.stateMu.RUnlock()
	return manager.repositoryState == "healthy" &&
		(!manager.InitialBackup || manager.initialState == "healthy") &&
		manager.fullState != "failed" && manager.diffState != "failed"
}

func (manager *Manager) Restore(ctx context.Context, pgData string, archiveEnabled bool, signals <-chan os.Signal) error {
	entries, err := os.ReadDir(pgData)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("PGBACKREST_RESTORE_ENABLED requires an empty PGDATA")
	}
	if err := os.MkdirAll(pgData, 0700); err != nil {
		return err
	}
	if err := os.Chown(filepath.Dir(pgData), 999, 999); err != nil {
		return err
	}
	if err := os.Chown(pgData, 999, 999); err != nil {
		return err
	}

	args := []string{"--stanza=" + manager.Stanza}
	if set := os.Getenv("PGBACKREST_RESTORE_SET"); set != "" {
		args = append(args, "--set="+set)
	}
	if target := os.Getenv("PGBACKREST_RESTORE_TARGET_TIME"); target != "" {
		args = append(args, "--type=time", "--target="+target, "--target-action=promote")
	}
	if !archiveEnabled {
		args = append(args, "--archive-mode=off")
	}
	args = append(args, "restore")
	if err := manager.runSupervised(ctx, signals, args...); err != nil {
		if cleanupErr := os.RemoveAll(pgData); cleanupErr != nil {
			return fmt.Errorf("%w; clean partial restore: %v", err, cleanupErr)
		}
		return err
	}
	return nil
}

func (manager *Manager) Start(ctx context.Context) {
	go func() {
		for {
			if err := manager.initialize(ctx); err != nil {
				fmt.Fprintf(os.Stdout, "pgBackRest initialization failed: %v\n", err)
				if !sleep(ctx, time.Minute) {
					return
				}
				continue
			}
			manager.schedule(ctx)
			return
		}
	}()
}

func (manager *Manager) initialize(ctx context.Context) error {
	if err := manager.run(ctx, "--stanza="+manager.Stanza, "stanza-create"); err != nil {
		if ctx.Err() == nil {
			_ = manager.setRepositoryState("failed")
		}
		return err
	}
	if err := manager.run(ctx, "--stanza="+manager.Stanza, "check"); err != nil {
		if ctx.Err() == nil {
			_ = manager.setRepositoryState("failed")
		}
		return err
	}
	_ = manager.setRepositoryState("healthy")
	if manager.InitialBackup {
		hasBackup, err := manager.hasBackup(ctx)
		if err != nil {
			if ctx.Err() == nil {
				_ = manager.setRepositoryState("failed")
				_ = manager.setInitialState("failed")
			}
			return err
		}
		if !hasBackup {
			if err := manager.runBackup(ctx, "full", "--type=full", "backup"); err != nil {
				if ctx.Err() == nil {
					_ = manager.setInitialState("failed")
				}
				return err
			}
		}
		_ = manager.setInitialState("healthy")
	}
	return nil
}

func (manager *Manager) hasBackup(ctx context.Context) (bool, error) {
	output, err := manager.output(ctx, "--stanza="+manager.Stanza, "--output=json", "info")
	if err != nil {
		return false, err
	}
	var info []struct {
		Backup []json.RawMessage `json:"backup"`
	}
	if err := json.Unmarshal(output, &info); err != nil {
		return false, err
	}
	return len(info) != 0 && len(info[0].Backup) != 0, nil
}

func (manager *Manager) schedule(ctx context.Context) {
	location, _ := time.LoadLocation(manager.Timezone)
	scheduler := cron.New(cron.WithLocation(location))
	for _, job := range []struct {
		schedule string
		args     []string
		state    string
	}{
		{manager.FullSchedule, []string{"--type=full", "backup"}, "full"},
		{manager.DiffSchedule, []string{"--type=diff", "backup"}, "differential"},
		{manager.CheckSchedule, []string{"check"}, "repository"},
	} {
		job := job
		if _, err := scheduler.AddFunc(job.schedule, func() {
			args := append([]string{"--stanza=" + manager.Stanza}, job.args...)
			if err := manager.runWithState(ctx, job.state, args...); err != nil {
				if ctx.Err() != nil {
					return
				}
				fmt.Fprintf(os.Stdout, "scheduled pgBackRest command failed: %v\n", err)
			}
		}); err != nil {
			fmt.Fprintf(os.Stdout, "invalid pgBackRest schedule %q: %v\n", job.schedule, err)
			_ = manager.setRepositoryState("failed")
			return
		}
	}
	scheduler.Start()
	<-ctx.Done()
	stop := scheduler.Stop()
	<-stop.Done()
}

func (manager *Manager) runBackup(ctx context.Context, state string, args ...string) error {
	return manager.runWithState(ctx, state, append([]string{"--stanza=" + manager.Stanza}, args...)...)
}

func (manager *Manager) setState(name, value string) error {
	manager.stateMu.Lock()
	switch name {
	case "repository":
		manager.repositoryState = value
	case "full":
		manager.fullState = value
	case "differential":
		manager.diffState = value
	case "initial":
		manager.initialState = value
	}
	manager.stateMu.Unlock()
	return status.Write(filepath.Join(manager.statePath, name), value)
}

func (manager *Manager) setRepositoryState(value string) error {
	return manager.setState("repository", value)
}

func (manager *Manager) setInitialState(value string) error {
	return manager.setState("initial", value)
}

func (manager *Manager) repositoryStatePath() string {
	return filepath.Join(manager.statePath, "repository")
}

func (manager *Manager) fullStatePath() string {
	return filepath.Join(manager.statePath, "full")
}

func (manager *Manager) diffStatePath() string {
	return filepath.Join(manager.statePath, "differential")
}

func (manager *Manager) run(ctx context.Context, args ...string) error {
	manager.commandMu.Lock()
	defer manager.commandMu.Unlock()
	return manager.runUnlocked(ctx, args...)
}

func (manager *Manager) runWithState(ctx context.Context, state string, args ...string) error {
	manager.commandMu.Lock()
	defer manager.commandMu.Unlock()
	err := manager.runUnlocked(ctx, args...)
	if ctx.Err() == nil {
		value := "healthy"
		if err != nil {
			value = "failed"
		}
		_ = manager.setState(state, value)
	}
	return err
}

func (manager *Manager) runUnlocked(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, "gosu", append([]string{"postgres", "pgbackrest"}, args...)...)
	command.Env = pgbackrestEnv()
	command.Stdout = os.Stdout
	command.Stderr = os.Stdout
	return command.Run()
}

func (manager *Manager) runSupervised(ctx context.Context, signals <-chan os.Signal, args ...string) error {
	manager.commandMu.Lock()
	defer manager.commandMu.Unlock()
	command := exec.Command("gosu", append([]string{"postgres", "pgbackrest"}, args...)...)
	command.Env = pgbackrestEnv()
	command.Stdout = os.Stdout
	command.Stderr = os.Stdout
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()
	contextDone := ctx.Done()
	var shutdownDeadline <-chan time.Time
	shutdownRequested := false
	for {
		select {
		case err := <-done:
			if err == nil && shutdownRequested {
				return fmt.Errorf("restore interrupted")
			}
			return err
		case received := <-signals:
			shutdownRequested = true
			_ = command.Process.Signal(received)
			if shutdownDeadline == nil {
				shutdownDeadline = time.After(30 * time.Second)
			}
		case <-contextDone:
			contextDone = nil
			shutdownRequested = true
			_ = command.Process.Signal(syscall.SIGTERM)
			shutdownDeadline = time.After(30 * time.Second)
		case <-shutdownDeadline:
			_ = command.Process.Kill()
			return <-done
		}
	}
}

func (manager *Manager) output(ctx context.Context, args ...string) ([]byte, error) {
	manager.commandMu.Lock()
	defer manager.commandMu.Unlock()
	command := exec.CommandContext(ctx, "gosu", append([]string{"postgres", "pgbackrest"}, args...)...)
	command.Env = pgbackrestEnv()
	command.Stderr = os.Stdout
	return command.Output()
}

func pgbackrestEnv() []string {
	values := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PGBACKREST_") {
			values = append(values, value)
		}
	}
	return values
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
