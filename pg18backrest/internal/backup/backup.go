package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jptrs93/container-images/pg18backrest/internal/config"
	"github.com/jptrs93/container-images/pg18backrest/internal/status"
	"github.com/robfig/cron/v3"
)

const (
	configPath = "/etc/pgbackrest/pgbackrest.conf"
	statePath  = "/var/lib/postgresql/pgbackrest-state"
	spoolPath  = "/var/lib/postgresql/pgbackrest-spool"
)

type Manager struct {
	Stanza         string
	InitialBackup  bool
	FullSchedule   string
	DiffSchedule   string
	CheckSchedule  string
	Timezone       string
	commandMu      sync.Mutex
	archiveTimeout string
}

func New(pgData, postgresUser string, options config.PGBackRestOptions) (*Manager, error) {
	if postgresUser == "" {
		postgresUser = "postgres"
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
	if options.S3VerifyTLS {
		verify = "y"
	}
	lines := []string{
		"[global]",
		"repo1-type=s3",
		"repo1-path=/",
		"repo1-s3-endpoint=" + options.S3Endpoint,
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
	lines = append(lines, "", "["+options.Stanza+"]", "pg1-path="+pgData, "pg1-port=5432", "pg1-user="+postgresUser)
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

	return &Manager{
		Stanza:         options.Stanza,
		InitialBackup:  options.InitialBackup,
		FullSchedule:   options.FullSchedule,
		DiffSchedule:   options.DiffSchedule,
		CheckSchedule:  options.CheckSchedule,
		Timezone:       options.Timezone,
		archiveTimeout: strconv.Itoa(options.ArchiveTimeoutSeconds),
	}, nil
}

func (manager *Manager) ArchiveCommand() string {
	return "pgbackrest --stanza=" + manager.Stanza + " archive-push %p"
}

func (manager *Manager) ArchiveTimeout() string {
	return manager.archiveTimeout
}

func (manager *Manager) Restore(ctx context.Context, pgData string, archiveEnabled bool) error {
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
	return manager.run(ctx, args...)
}

func (manager *Manager) Start(ctx context.Context) {
	go func() {
		for {
			if err := manager.initialize(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "pgBackRest initialization failed: %v\n", err)
				_ = status.Write(statePath, "failed")
				if !sleep(ctx, time.Minute) {
					return
				}
				continue
			}
			_ = status.Write(statePath, "healthy")
			manager.schedule(ctx)
			return
		}
	}()
}

func (manager *Manager) initialize(ctx context.Context) error {
	if err := manager.run(ctx, "--stanza="+manager.Stanza, "stanza-create"); err != nil {
		return err
	}
	if err := manager.run(ctx, "--stanza="+manager.Stanza, "check"); err != nil {
		return err
	}
	if manager.InitialBackup {
		hasBackup, err := manager.hasBackup(ctx)
		if err != nil {
			return err
		}
		if !hasBackup {
			return manager.run(ctx, "--stanza="+manager.Stanza, "--type=full", "backup")
		}
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
	location, err := time.LoadLocation(manager.Timezone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid backup timezone: %v\n", err)
		_ = status.Write(statePath, "failed")
		return
	}
	scheduler := cron.New(cron.WithLocation(location))
	for _, job := range []struct {
		schedule string
		args     []string
	}{
		{manager.FullSchedule, []string{"--type=full", "backup"}},
		{manager.DiffSchedule, []string{"--type=diff", "backup"}},
		{manager.CheckSchedule, []string{"check"}},
	} {
		job := job
		if _, err := scheduler.AddFunc(job.schedule, func() {
			args := append([]string{"--stanza=" + manager.Stanza}, job.args...)
			if err := manager.run(context.Background(), args...); err != nil {
				fmt.Fprintf(os.Stderr, "scheduled pgBackRest command failed: %v\n", err)
				_ = status.Write(statePath, "failed")
				return
			}
			_ = status.Write(statePath, "healthy")
		}); err != nil {
			fmt.Fprintf(os.Stderr, "invalid pgBackRest schedule %q: %v\n", job.schedule, err)
			_ = status.Write(statePath, "failed")
			return
		}
	}
	scheduler.Start()
	<-ctx.Done()
	stop := scheduler.Stop()
	<-stop.Done()
}

func (manager *Manager) run(ctx context.Context, args ...string) error {
	manager.commandMu.Lock()
	defer manager.commandMu.Unlock()
	command := exec.CommandContext(ctx, "gosu", append([]string{"postgres", "pgbackrest"}, args...)...)
	command.Env = pgbackrestEnv()
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func (manager *Manager) output(ctx context.Context, args ...string) ([]byte, error) {
	manager.commandMu.Lock()
	defer manager.commandMu.Unlock()
	command := exec.CommandContext(ctx, "gosu", append([]string{"postgres", "pgbackrest"}, args...)...)
	command.Env = pgbackrestEnv()
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
