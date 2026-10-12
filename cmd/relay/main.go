// Command relay runs the webhook relay's receiver or forwarder.
//
//	relay receiver [-config path] [-listen addr]
//	relay forwarder [-config path]
//
// Without -config, the configuration is read from the RELAY_CONFIG
// environment variable, which holds the file's content.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NiekKeijzer/webhook-relay/config"
)

// configEnv holds the configuration's content when -config is not given.
const configEnv = "RELAY_CONFIG"

const usage = `usage:
  relay receiver [-config path] [-listen addr]
  relay forwarder [-config path]

Without -config, the configuration is read from ` + configEnv + `.
`

// errUsage marks errors in the command line.
var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.LookupEnv, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, lookupEnv config.LookupFunc, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	var err error
	switch {
	case len(args) > 0 && args[0] == "receiver":
		err = runReceiver(ctx, args[1:], lookupEnv, logger, stderr)
	case len(args) > 0 && args[0] == "forwarder":
		err = runForwarder(ctx, args[1:], lookupEnv, logger, stderr)
	default:
		err = errUsage
	}
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprint(stderr, usage)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "relay: %v\n", err)
		return 1
	}
	return 0
}

func runReceiver(ctx context.Context, args []string, lookupEnv config.LookupFunc, logger *slog.Logger, stderr io.Writer) error {
	fs := flag.NewFlagSet("receiver", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path of the configuration file")
	listen := fs.String("listen", ":8080", "address to serve HTTP on")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return errUsage
	}
	regs, err := newRegistries(queueTypes)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath, lookupEnv, regs)
	if err != nil {
		return err
	}
	env, err := cfg.ResolveReceiver(lookupEnv, regs)
	if err != nil {
		return err
	}
	sender, err := regs.queues[env.Queue.Type].NewSender(ctx, env.Queue.Params, env.Queue.Credentials)
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	h, err := newReceiver(cfg, regs, env, sender, logger)
	if err != nil {
		return err
	}
	return serve(ctx, *listen, h, logger)
}

func runForwarder(ctx context.Context, args []string, lookupEnv config.LookupFunc, logger *slog.Logger, stderr io.Writer) error {
	fs := flag.NewFlagSet("forwarder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path of the configuration file")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		return errUsage
	}
	regs, err := newRegistries(queueTypes)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath, lookupEnv, regs)
	if err != nil {
		return err
	}
	env, err := config.ResolveForwarder(lookupEnv, regs)
	if err != nil {
		return err
	}
	receiver, err := regs.queues[env.Queue.Type].NewReceiver(ctx, env.Queue.Params, env.Queue.Credentials)
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	f, err := newForwarder(cfg, env, receiver, logger)
	if err != nil {
		return err
	}
	return f.Run(ctx)
}

// loadConfig reads the configuration from path, or from configEnv when path
// is empty, and parses and validates it.
func loadConfig(path string, lookupEnv config.LookupFunc, plugins config.Plugins) (*config.Config, error) {
	content, inEnv := lookupEnv(configEnv)
	inEnv = inEnv && content != ""
	var data []byte
	switch {
	case path != "" && inEnv:
		return nil, fmt.Errorf("set either -config or %s, not both", configEnv)
	case path != "":
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, err
		}
	case inEnv:
		data = []byte(content)
	default:
		return nil, fmt.Errorf("no configuration: set -config or %s", configEnv)
	}
	return config.Parse(data, plugins)
}

// serve serves h on addr until ctx is cancelled, then shuts down gracefully.
func serve(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
