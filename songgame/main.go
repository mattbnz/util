package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	redirectBase := flag.String("redirect-base", "http://127.0.0.1:8080", "OAuth redirect base URL (must match Spotify app settings)")
	statePath := flag.String("state", "songgame-state.json", "path to state file (set empty to disable persistence)")
	flag.Parse()

	clientID := os.Getenv("SPOTIFY_CLIENT_ID")
	clientSecret := os.Getenv("SPOTIFY_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		fmt.Fprintln(os.Stderr, "SPOTIFY_CLIENT_ID and SPOTIFY_CLIENT_SECRET env vars are required.")
		fmt.Fprintln(os.Stderr, "Register an app at https://developer.spotify.com/dashboard and add redirect URI:")
		fmt.Fprintln(os.Stderr, "  "+*redirectBase+"/admin/callback")
		os.Exit(1)
	}

	srv := NewServer(ServerConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  *redirectBase + "/admin/callback",
		BaseURL:      *redirectBase,
	})

	var store *Store
	stopStore := make(chan struct{})
	if *statePath != "" {
		store = NewStore(*statePath, srv.Game())
		if _, err := os.Stat(*statePath); err == nil {
			if err := store.Load(); err != nil {
				log.Printf("state load (%s): %v", *statePath, err)
			} else {
				log.Printf("state loaded from %s", *statePath)
			}
		} else {
			log.Printf("state file: %s (new)", *statePath)
		}
		srv.Game().SetChangeCallback(store.MarkDirty)
		go store.Run(2*time.Second, stopStore)
	}

	log.Printf("songgame listening on %s", *addr)
	log.Printf("players join at: %s/", *redirectBase)

	httpSrv := &http.Server{Addr: *addr, Handler: srv}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// Block until SIGINT/SIGTERM, then flush state and exit.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if store != nil {
		close(stopStore)
		// best-effort final save; the Run loop's final flush handles normal cases,
		// but an explicit call covers the race where the signal arrives mid-tick.
		if err := store.Save(); err != nil {
			log.Printf("final save: %v", err)
		}
	}
}
