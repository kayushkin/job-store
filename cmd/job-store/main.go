package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	jobstore "github.com/kayushkin/job-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	settings, err := jobstore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	addr := settings.String(jobstore.SettingListenAddress)

	store, err := jobstore.Open(settings.String(jobstore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()
	store.SetNoteboardBaseURL(settings.String(jobstore.SettingNoteboardURL))

	mux := http.NewServeMux()
	jobstore.RegisterHandlers(mux, store)
	jobstore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("job-store listening on %s (data=%s)", addr, store.DataDir())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
