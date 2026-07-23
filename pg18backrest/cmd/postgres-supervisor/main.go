package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jptrs93/container-images/pg18backrest/internal/backup"
	"github.com/jptrs93/container-images/pg18backrest/internal/config"
	"github.com/jptrs93/container-images/pg18backrest/internal/reconciler"
	"github.com/jptrs93/container-images/pg18backrest/internal/status"
)

const supervisorStatePath = "/var/lib/postgresql/supervisor-state"

func main() {
	log.SetOutput(os.Stdout)
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stdout, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stdout, err)
		os.Exit(exitCode(err))
	}
}

func run() error {
	arguments := os.Args[1:]
	if len(arguments) == 0 {
		arguments = []string{"postgres"}
	}
	if arguments[0] != "postgres" && !strings.HasPrefix(arguments[0], "-") {
		return runUpstream(arguments)
	}
	if strings.HasPrefix(arguments[0], "-") {
		arguments = append([]string{"postgres"}, arguments...)
	}

	pgData := envOr("PGDATA", "/var/lib/postgresql/18/docker")
	cfg, err := config.Load(envOr("POSTGRES_SUPERVISOR_CONFIG", "/etc/postgres-supervisor/config.yaml"))
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	var initDB *config.InitDBOptions
	if cfg.InitDB != nil {
		options, err := cfg.InitDB.Resolve()
		if err != nil {
			return err
		}
		initDB = &options
	}

	backupEnabled := cfg.PGBackRest != nil && cfg.PGBackRest.Enabled
	restore := restoreEnabled()
	var manager *backup.Manager
	var backupErr error
	if backupEnabled || restore {
		if cfg.PGBackRest == nil {
			backupErr = errors.New("pgbackrest configuration is required")
		} else {
			options, err := cfg.PGBackRest.Resolve()
			if err == nil {
				manager, err = backup.New(pgData, supervisorAdminUser(initDB), options)
			}
			backupErr = err
		}
	}
	if backupErr != nil {
		log.Printf("pgBackRest is disabled because its configuration is invalid: %v", backupErr)
		_ = status.Write("/var/lib/postgresql/pgbackrest-state", "failed")
	}
	_ = status.Write(supervisorStatePath, "starting")
	if backupEnabled && backupErr == nil {
		_ = status.Write("/var/lib/postgresql/pgbackrest-state", "starting")
	}
	if restore {
		if manager == nil {
			return errors.New("cannot restore without valid pgBackRest configuration")
		}
		if err := manager.Restore(context.Background(), pgData, backupEnabled); err != nil {
			return fmt.Errorf("restore PostgreSQL data: %w", err)
		}
	}

	options := config.RuntimeOptions{
		PGData:          pgData,
		ConfigDirectory: "/etc/postgres-supervisor",
	}
	if manager != nil && backupEnabled {
		options.ArchiveCommand = manager.ArchiveCommand()
		options.ArchiveTimeout = manager.ArchiveTimeout()
	}
	if err := config.WritePostgresFiles(cfg, options); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := startHealthServer()
	defer shutdownHealthServer(server)

	commandArgs := append(arguments, "-c", "config_file=/etc/postgres-supervisor/postgresql.conf")
	command := exec.Command("docker-entrypoint.sh", commandArgs...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stdout
	command.Stdin = os.Stdin
	command.Env = postgresEnv(cfg, initDB)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start PostgreSQL: %w", err)
	}

	signals := make(chan os.Signal, 1)
	shutdownRequested := make(chan struct{})
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case signal := <-signals:
			close(shutdownRequested)
			_ = command.Process.Signal(signal)
		case <-ctx.Done():
		}
	}()

	go reconcileAndBackup(ctx, cfg, initDB, manager, backupEnabled)
	err = command.Wait()
	cancel()
	if err == nil {
		select {
		case <-shutdownRequested:
		default:
			return errors.New("PostgreSQL exited unexpectedly")
		}
	}
	return err
}

func reconcileAndBackup(ctx context.Context, cfg config.Config, initDB *config.InitDBOptions, manager *backup.Manager, backupEnabled bool) {
	for {
		if err := waitForPostgres(ctx, supervisorAdminUser(initDB)); err != nil {
			return
		}
		if err := reconciler.Reconcile(ctx, cfg, initDB); err != nil {
			log.Printf("PostgreSQL reconciliation failed: %v", err)
			_ = status.Write(supervisorStatePath, "failed")
			if !sleep(ctx, 30*time.Second) {
				return
			}
			continue
		}
		_ = status.Write(supervisorStatePath, "healthy")
		if manager != nil && backupEnabled {
			manager.Start(ctx)
		}
		return
	}
}

func waitForPostgres(ctx context.Context, user string) error {
	for {
		command := exec.CommandContext(ctx, "pg_isready", "-q", "-U", user, "-d", "postgres")
		if command.Run() == nil {
			return nil
		}
		if !sleep(ctx, time.Second) {
			return ctx.Err()
		}
	}
}

func startHealthServer() *http.Server {
	server := &http.Server{
		Addr: envOr("POSTGRES_SUPERVISOR_HEALTH_ADDR", ":8081"),
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/healthz" {
				http.NotFound(writer, request)
				return
			}
			if err := healthcheck(); err != nil {
				http.Error(writer, err.Error(), http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("ok\n"))
		}),
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health server failed: %v", err)
		}
	}()
	return server
}

func shutdownHealthServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func healthcheck() error {
	if err := exec.Command("pg_isready", "-q", "-U", supervisorAdminUser(nil), "-d", "postgres").Run(); err != nil {
		return errors.New("PostgreSQL is not ready")
	}
	if !status.Healthy(supervisorStatePath) {
		return errors.New("PostgreSQL reconciliation is not healthy")
	}
	if pgBackRestEnabled() && !status.Healthy("/var/lib/postgresql/pgbackrest-state") {
		return errors.New("pgBackRest is not healthy")
	}
	return nil
}

func runUpstream(arguments []string) error {
	command := exec.Command("docker-entrypoint.sh", arguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stdout
	command.Stdin = os.Stdin
	return command.Run()
}

func pgBackRestEnabled() bool {
	cfg, err := config.Load(envOr("POSTGRES_SUPERVISOR_CONFIG", "/etc/postgres-supervisor/config.yaml"))
	return err == nil && cfg.PGBackRest != nil && cfg.PGBackRest.Enabled
}

func restoreEnabled() bool {
	return os.Getenv("PGBACKREST_RESTORE_ENABLED") == "true"
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func supervisorAdminUser(initDB *config.InitDBOptions) string {
	if user := os.Getenv("POSTGRES_SUPERVISOR_ADMIN_USER"); user != "" {
		return user
	}
	if initDB != nil {
		return initDB.PostgresUser
	}
	if cfg, err := config.Load(envOr("POSTGRES_SUPERVISOR_CONFIG", "/etc/postgres-supervisor/config.yaml")); err == nil && cfg.InitDB != nil {
		if user, err := cfg.InitDB.ResolveUser(); err == nil {
			return user
		}
	}
	if user := os.Getenv("POSTGRES_USER"); user != "" {
		return user
	}
	return "postgres"
}

func postgresEnv(cfg config.Config, initDB *config.InitDBOptions) []string {
	excluded := map[string]struct{}{}
	for _, name := range cfg.EnvironmentReferences() {
		excluded[name] = struct{}{}
	}
	if initDB != nil {
		for _, name := range []string{"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB"} {
			excluded[name] = struct{}{}
		}
	}
	values := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "PGBACKREST_") {
			if _, ok := excluded[name]; !ok {
				values = append(values, value)
			}
		}
	}
	if initDB != nil {
		values = append(values,
			"POSTGRES_USER="+initDB.PostgresUser,
			"POSTGRES_PASSWORD="+initDB.PostgresPassword,
			"POSTGRES_DB="+initDB.PostgresDB,
		)
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

func exitCode(err error) int {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return 1
}
