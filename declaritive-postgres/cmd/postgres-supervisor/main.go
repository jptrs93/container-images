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

	"github.com/jptrs93/container-images/declaritive-postgres/internal/config"
	"github.com/jptrs93/container-images/declaritive-postgres/internal/reconciler"
	"github.com/jptrs93/container-images/declaritive-postgres/internal/status"
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

	if err := status.Write(supervisorStatePath, "starting"); err != nil {
		return fmt.Errorf("write supervisor startup state: %w", err)
	}
	if err := config.WritePostgresFiles(cfg, config.RuntimeOptions{
		PGData:          pgData,
		ConfigDirectory: "/etc/postgres-supervisor",
	}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := startHealthServer(func() error {
		return healthcheck(adminUser)
	})
	if err != nil {
		return err
	}
	defer shutdownHealthServer(server)

	commandArgs := append(arguments,
		"-c", "config_file=/etc/postgres-supervisor/postgresql.conf",
		"-c", "listen_addresses=*",
		"-c", "port=5432",
		"-c", "unix_socket_directories=/var/run/postgresql",
	)
	command := exec.Command("docker-entrypoint.sh", commandArgs...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stdout
	command.Stdin = os.Stdin
	command.Env = postgresEnv(cfg, initDB)
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
		if err := reconcile(ctx, cfg, initDB, adminUser); err != nil && ctx.Err() == nil {
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

func reconcile(ctx context.Context, cfg config.Config, initDB *config.InitDBOptions, adminUser string) error {
	if err := waitForPostgres(ctx, adminUser); err != nil {
		return err
	}
	if err := reconciler.Reconcile(ctx, cfg, initDB, adminUser); err != nil {
		return err
	}
	if err := status.Write(supervisorStatePath, "healthy"); err != nil {
		return fmt.Errorf("write successful reconciliation state: %w", err)
	}
	return nil
}

func waitForPostgres(ctx context.Context, user string) error {
	for {
		command := exec.CommandContext(ctx, "pg_isready", "-q", "-h", "/var/run/postgresql", "-p", "5432", "-U", user, "-d", "postgres")
		if command.Run() == nil {
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

func healthcheck(adminUser string) error {
	if err := exec.Command("pg_isready", "-q", "-h", "/var/run/postgresql", "-p", "5432", "-U", adminUser, "-d", "postgres").Run(); err != nil {
		return errors.New("PostgreSQL is not ready")
	}
	if !status.Healthy(supervisorStatePath) {
		return errors.New("PostgreSQL reconciliation is not healthy")
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
		if _, ok := excluded[name]; !ok {
			values = append(values, value)
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
