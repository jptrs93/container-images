package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jptrs93/container-images/declarative-postgres-backrest/internal/backup"
	"github.com/jptrs93/container-images/declarative-postgres-backrest/internal/config"
	"github.com/jptrs93/container-images/declarative-postgres-backrest/internal/reconciler"
	"github.com/jptrs93/container-images/declarative-postgres-backrest/internal/status"
)

const supervisorStatePath = "/var/lib/postgresql/supervisor-state"

func main() {
	log.SetOutput(os.Stdout)
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := remoteHealthcheck(); err != nil {
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
	connection := cfg.ConnectionOptions()
	var initDB *config.InitDBOptions
	if cfg.InitDB != nil {
		options, err := cfg.InitDB.Resolve()
		if err != nil {
			return err
		}
		initDB = &options
	}
	adminUser, err := supervisorAdminUser(initDB)
	if err != nil {
		return err
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
				manager, err = backup.New(pgData, adminUser, connection, options)
			}
			backupErr = err
		}
	}
	if backupErr != nil {
		return fmt.Errorf("configure pgBackRest: %w", backupErr)
	}
	if err := status.Write(supervisorStatePath, "starting"); err != nil {
		return fmt.Errorf("write supervisor startup state: %w", err)
	}
	if manager != nil && backupEnabled {
		if err := manager.SetStarting(); err != nil {
			return fmt.Errorf("write pgBackRest startup state: %w", err)
		}
	}
	if restore {
		if manager == nil {
			return errors.New("cannot restore without valid pgBackRest configuration")
		}
		restoreSignals := make(chan os.Signal, 2)
		signal.Notify(restoreSignals, syscall.SIGINT, syscall.SIGTERM)
		err := manager.Restore(context.Background(), pgData, backupEnabled, restoreSignals)
		signal.Stop(restoreSignals)
		if err != nil {
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
	server, err := startHealthServer(func() error {
		return healthcheck(connection, adminUser, manager, backupEnabled)
	})
	if err != nil {
		return err
	}
	defer shutdownHealthServer(server)

	commandArgs := append(arguments,
		"-c", "config_file=/etc/postgres-supervisor/postgresql.conf",
		"-c", "listen_addresses="+connection.ListenAddresses,
		"-c", "port="+connection.Port,
		"-c", "unix_socket_directories="+connection.SocketDirectories,
	)
	if backupEnabled {
		commandArgs = append(commandArgs,
			"-c", "archive_mode=on",
			"-c", "archive_command="+manager.ArchiveCommand(),
			"-c", "archive_library=",
			"-c", "archive_timeout="+manager.ArchiveTimeout(),
		)
	} else {
		commandArgs = append(commandArgs,
			"-c", "archive_mode=off",
			"-c", "archive_command=",
			"-c", "archive_library=",
			"-c", "archive_timeout=60",
		)
	}
	command := exec.Command("docker-entrypoint.sh", commandArgs...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stdout
	command.Stdin = os.Stdin
	command.Env = postgresChildEnv(cfg, initDB, connection)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start PostgreSQL: %w", err)
	}

	processDone := make(chan error, 1)
	go func() {
		processDone <- command.Wait()
	}()

	reconciliationFailed := make(chan error, 1)
	go func() {
		if err := reconcileAndBackup(ctx, cfg, initDB, connection, adminUser, manager, backupEnabled); err != nil && ctx.Err() == nil {
			reconciliationFailed <- err
		}
	}()

	shutdownRequested := false
	var shutdownDeadline <-chan time.Time
	for {
		select {
		case received := <-signals:
			if !shutdownRequested {
				shutdownRequested = true
				cancel()
				shutdownDeadline = time.After(30 * time.Second)
			}
			_ = command.Process.Signal(received)
		case err := <-reconciliationFailed:
			if shutdownRequested {
				continue
			}
			log.Printf("PostgreSQL reconciliation failed: %v", err)
			_ = status.Write(supervisorStatePath, "failed")
			cancel()
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-processDone:
			case <-time.After(30 * time.Second):
				_ = command.Process.Kill()
				<-processDone
			}
			return fmt.Errorf("PostgreSQL reconciliation failed: %w", err)
		case err := <-processDone:
			cancel()
			if err == nil && !shutdownRequested {
				return errors.New("PostgreSQL exited unexpectedly")
			}
			return err
		case <-shutdownDeadline:
			_ = command.Process.Kill()
			shutdownDeadline = nil
		}
	}
}

func reconcileAndBackup(ctx context.Context, cfg config.Config, initDB *config.InitDBOptions, connection config.ConnectionOptions, adminUser string, manager *backup.Manager, backupEnabled bool) error {
	if err := waitForPostgres(ctx, connection, adminUser); err != nil {
		return err
	}
	if err := reconciler.Reconcile(ctx, cfg, initDB, connection, adminUser); err != nil {
		return err
	}
	if err := status.Write(supervisorStatePath, "healthy"); err != nil {
		return fmt.Errorf("write successful reconciliation state: %w", err)
	}
	if manager != nil && backupEnabled {
		manager.Start(ctx)
	}
	return nil
}

func waitForPostgres(ctx context.Context, connection config.ConnectionOptions, user string) error {
	for {
		command := exec.CommandContext(ctx, "psql", "-qAt", "-h", connection.SocketDirectory, "-p", connection.Port, "-U", user, "-d", "postgres", "-c", "select not pg_is_in_recovery()")
		output, err := command.Output()
		if err == nil && strings.TrimSpace(string(output)) == "t" {
			return nil
		}
		if !sleep(ctx, time.Second) {
			return ctx.Err()
		}
	}
}

func startHealthServer(check func() error) (*http.Server, error) {
	server := &http.Server{
		Addr: envOr("POSTGRES_SUPERVISOR_HEALTH_ADDR", ":8081"),
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/healthz" {
				http.NotFound(writer, request)
				return
			}
			if err := check(); err != nil {
				http.Error(writer, err.Error(), http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("ok\n"))
		}),
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return nil, fmt.Errorf("start health server: %w", err)
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health server failed: %v", err)
		}
	}()
	return server, nil
}

func shutdownHealthServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func healthcheck(connection config.ConnectionOptions, adminUser string, manager *backup.Manager, backupEnabled bool) error {
	if err := exec.Command("pg_isready", "-q", "-h", connection.SocketDirectory, "-p", connection.Port, "-U", adminUser, "-d", "postgres").Run(); err != nil {
		return errors.New("PostgreSQL is not ready")
	}
	if !status.Healthy(supervisorStatePath) {
		return errors.New("PostgreSQL reconciliation is not healthy")
	}
	if backupEnabled && (manager == nil || !manager.Healthy()) {
		return errors.New("pgBackRest is not healthy")
	}
	return nil
}

func remoteHealthcheck() error {
	client := http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(healthURL())
	if err != nil {
		return fmt.Errorf("query supervisor health: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return nil
	}
	contents, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	message := strings.TrimSpace(string(contents))
	if message == "" {
		message = response.Status
	}
	return errors.New(message)
}

func healthURL() string {
	address := envOr("POSTGRES_SUPERVISOR_HEALTH_ADDR", ":8081")
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "http://127.0.0.1:8081/healthz"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

func runUpstream(arguments []string) error {
	command := exec.Command("docker-entrypoint.sh", arguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stdout
	command.Stdin = os.Stdin
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()
	for {
		select {
		case received := <-signals:
			_ = command.Process.Signal(received)
		case err := <-done:
			return err
		}
	}
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

func supervisorAdminUser(initDB *config.InitDBOptions) (string, error) {
	if user := os.Getenv("POSTGRES_SUPERVISOR_ADMIN_USER"); user != "" {
		return user, nil
	}
	if initDB != nil {
		return initDB.PostgresUser, nil
	}
	return config.ResolveEnvironmentOrFile("POSTGRES_USER", "postgres")
}

func postgresEnv(cfg config.Config, initDB *config.InitDBOptions) []string {
	excluded := map[string]struct{}{}
	for _, name := range cfg.EnvironmentReferences() {
		if isOfficialPostgresEnvironment(name) && (initDB == nil || !isDirectInitDBEnvironment(name)) {
			continue
		}
		excluded[name] = struct{}{}
	}
	if initDB != nil {
		for _, name := range []string{"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB", "POSTGRES_USER_FILE", "POSTGRES_PASSWORD_FILE", "POSTGRES_DB_FILE"} {
			excluded[name] = struct{}{}
		}
	}
	for _, name := range []string{"PGHOST", "PGHOSTADDR", "PGPORT", "PGSERVICE", "PGSERVICEFILE"} {
		excluded[name] = struct{}{}
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

func postgresChildEnv(cfg config.Config, initDB *config.InitDBOptions, connection config.ConnectionOptions) []string {
	return append(postgresEnv(cfg, initDB),
		"PGHOST="+connection.SocketDirectory,
		"PGPORT="+connection.Port,
		"PGSERVICE=postgres-supervisor",
		"PGSERVICEFILE=/etc/postgres-supervisor/pg_service.conf",
	)
}

func isOfficialPostgresEnvironment(name string) bool {
	return strings.HasPrefix(name, "POSTGRES_")
}

func isDirectInitDBEnvironment(name string) bool {
	switch name {
	case "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB":
		return true
	default:
		return false
	}
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
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitError.ExitCode()
	}
	return 1
}
