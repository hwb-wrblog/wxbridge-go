package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"feiyu.com/wx/internal/httpapi"
	"feiyu.com/wx/internal/workflow"
)

func runServer(s *workflow.Session, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	auto := flags.Bool("auto-download", true, "automatically download newly received media")
	interval := flags.Duration("sync-interval", 5*time.Second, "background message synchronization interval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected serve arguments")
	}
	if *interval < time.Second {
		return errors.New("sync interval must be at least one second")
	}
	token, err := httpapi.TokenFile(s.Dir)
	if err != nil {
		return err
	}
	server, err := httpapi.New(s, httpapi.Config{Listen: *listen, Token: token, SyncInterval: *interval, AutoDownload: *auto})
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return server.Run(ctx)
}
