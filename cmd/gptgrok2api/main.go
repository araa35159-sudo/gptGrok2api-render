package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/githubbackup"
	"github.com/auucoder/gptgrok2api-go/internal/httpapi"
)

func main() {
	cfg, err := config.Load("")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	backup, err := githubbackup.FromEnv()
	if err != nil {
		log.Fatalf("configure GitHub backup: %v", err)
	}
	if backup != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		restored, restoreErr := backup.Restore(ctx, cfg)
		cancel()
		if restoreErr != nil {
			log.Fatalf("restore GitHub backup: %v", restoreErr)
		}
		if restored {
			log.Print("restored encrypted state from GitHub")
		}
		// Settings in the restored config must be applied before providers start.
		cfg, err = config.Load("")
		if err != nil {
			log.Fatalf("load restored config: %v", err)
		}
	}
	logFile, logErr := os.OpenFile(filepath.Join(cfg.RootDir, "logs", "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if logErr == nil {
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	} else {
		log.Printf("open runtime log: %v", logErr)
	}

	app := httpapi.New(cfg)
	backupCtx, stopBackup := context.WithCancel(context.Background())
	if backup != nil {
		go backup.Run(backupCtx, cfg, func(err error) { log.Printf("sync GitHub backup: %v", err) })
	}
	server := &http.Server{
		Addr:                         cfg.ListenAddr,
		Handler:                      app.Handler(),
		ReadHeaderTimeout:            10 * time.Second,
		ReadTimeout:                  cfg.RequestTimeout,
		WriteTimeout:                 0,
		IdleTimeout:                  120 * time.Second,
		MaxHeaderBytes:               1 << 20,
		DisableGeneralOptionsHandler: false,
		// Streaming endpoints deliberately keep WriteTimeout disabled.
	}

	go func() {
		log.Printf("gptgrok2api-go listening on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	stopBackup()
	if backup != nil {
		if err := app.FlushPersistentState(); err != nil {
			log.Printf("flush local state: %v", err)
		}
		backupCtx, backupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer backupCancel()
		if _, err := backup.Sync(backupCtx, cfg); err != nil {
			log.Printf("final GitHub backup: %v", err)
		}
	}
}
