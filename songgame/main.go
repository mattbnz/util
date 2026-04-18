package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	redirectBase := flag.String("redirect-base", "http://127.0.0.1:8080", "OAuth redirect base URL (must match Spotify app settings)")
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
	})

	log.Printf("songgame listening on %s", *addr)
	log.Printf("admin:   %s/admin", *redirectBase)
	log.Printf("players: %s/", *redirectBase)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatal(err)
	}
}
