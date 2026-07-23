package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

func New(pgData string) (*Manager, error) {
	enabled, err := boolEnv("PGBACKREST_ENABLED", false)
	if err != nil {
		return nil, err
	}
	restore, err := boolEnv("PGBACKREST_RESTORE_ENABLED", false)
	if err != nil {
		return nil, err
	}
	if !enabled && !restore {
		return nil, nil
	}

	stanza, err := requiredEnv("PGBACKREST_STANZA")
	if err != nil {
		return nil, err
	}
	endpoint, err := requiredEnv("PGBACKREST_S3_ENDPOINT")
	if err != nil {
		return nil, err
	}
	bucket, err := requiredEnv("PGBACKREST_S3_BUCKET")
	if err != nil {
		return nil, err
	}
	key, err := fileEnv("PGBACKREST_S3_KEY")
	if err != nil || key == "" {
		return nil, requiredError("PGBACKREST_S3_KEY", err)
	}
	secret, err := fileEnv("PGBACKREST_S3_KEY_SECRET")
	if err != nil || secret == "" {
		return nil, requiredError("PGBACKREST_S3_KEY_SECRET", err)
	}
	verifyTLS, err := boolEnv("PGBACKREST_S3_VERIFY_TLS", true)
	if err != nil {
		return nil, err
	}
	uriStyle := envOr("PGBACKREST_S3_URI_STYLE", "path")
	if uriStyle != "path" && uriStyle != "host" {
		return nil, fmt.Errorf("PGBACKREST_S3_URI_STYLE must be path or host")
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
	if verifyTLS {
		verify = "y"
	}
	lines := []string{
		"[global]",
		"repo1-type=s3",
		"repo1-path=/",
		"repo1-s3-endpoint=" + endpoint,
		"repo1-s3-bucket=" + bucket,
		"repo1-s3-region=" + envOr("PGBACKREST_S3_REGION", "us-east-1"),
		"repo1-s3-key=" + key,
		"repo1-s3-key-secret=" + secret,
		"repo1-s3-uri-style=" + uriStyle,
		"repo1-storage-verify-tls=" + verify,
		"repo1-retention-full=" + envOr("PGBACKREST_RETENTION_FULL", "2"),
		"repo1-retention-archive=" + envOr("PGBACKREST_RETENTION_ARCHIVE", "2"),
		"archive-async=y",
		"archive-push-queue-max=" + envOr("PGBACKREST_ARCHIVE_PUSH_QUEUE_MAX", "1GiB"),
		"archive-timeout=" + envOr("PGBACKREST_ARCHIVE_TIMEOUT", "60"),
		"process-max=" + envOr("PGBACKREST_PROCESS_MAX", "4"),
		"compress-type=zst",
		"repo1-bundle=y",
		"repo1-block=y",
		"start-fast=y",
		"log-level-console=info",
		"log-level-file=detail",
		"spool-path=" + spoolPath,
	}
	if cipher, err := fileEnv("PGBACKREST_REPO_CIPHER_PASS"); err != nil {
		return nil, err
	} else if cipher != "" {
		lines = append(lines, "repo1-cipher-type=aes-256-cbc", "repo1-cipher-pass="+cipher)
	}
	lines = append(lines, "", "["+stanza+"]", "pg1-path="+pgData, "pg1-port=5432", "pg1-user=postgres")
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
		Stanza:         stanza,
		InitialBackup:  envOr("PGBACKREST_INITIAL_BACKUP", "true") == "true",
		FullSchedule:   envOr("PGBACKREST_FULL_SCHEDULE", "0 2 * * 0"),
		DiffSchedule:   envOr("PGBACKREST_DIFF_SCHEDULE", "0 2 * * 1-6"),
		CheckSchedule:  envOr("PGBACKREST_CHECK_SCHEDULE", "*/5 * * * *"),
		Timezone:       envOr("TZ", "UTC"),
		archiveTimeout: envOr("PGBACKREST_ARCHIVE_TIMEOUT", "60"),
	}, nil
}

func (manager *Manager) ArchiveCommand() string {
	return "pgbackrest --stanza=" + manager.Stanza + " archive-push %p"
}

func (manager *Manager) ArchiveTimeout() string {
	return manager.archiveTimeout
}

func (manager *Manager) Restore(ctx context.Context, pgData string) error {
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
	enabled, _ := boolEnv("PGBACKREST_ENABLED", false)
	if !enabled {
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

func fileEnv(name string) (string, error) {
	value := os.Getenv(name)
	file := os.Getenv(name + "_FILE")
	if value != "" && file != "" {
		return "", fmt.Errorf("%s and %s_FILE cannot both be set", name, name)
	}
	if file == "" {
		return value, nil
	}
	contents, err := os.ReadFile(file)
	return strings.TrimSuffix(string(contents), "\n"), err
}

func requiredEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s must be set", name)
	}
	return value, nil
}

func requiredError(name string, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%s must be set", name)
}

func boolEnv(name string, defaultValue bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue, nil
	}
	if value == "true" {
		return true, nil
	}
	if value == "false" {
		return false, nil
	}
	return false, fmt.Errorf("%s must be true or false", name)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
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
