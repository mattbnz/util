package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

type ServerConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

type Server struct {
	cfg     ServerConfig
	spotify *SpotifyClient
	game    *Game
	tpl     *template.Template

	mu         sync.Mutex
	oauthState string
	adminToken string // cookie value required for /admin/*
}

func NewServer(cfg ServerConfig) *Server {
	funcs := template.FuncMap{
		"inc": func(i int) int { return i + 1 },
	}
	tpl := template.Must(template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
	s := &Server{
		cfg:     cfg,
		spotify: NewSpotifyClient(cfg.ClientID, cfg.ClientSecret, cfg.RedirectURI),
		game:    NewGame(),
		tpl:     tpl,
	}
	s.adminToken = randomHex(16)
	log.Printf("admin token (auto-set on first visit): %s", s.adminToken)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/":
		s.handlePlayerRoot(w, r)
	case r.URL.Path == "/join":
		s.handleJoin(w, r)
	case r.URL.Path == "/answer":
		s.handleAnswer(w, r)
	case r.URL.Path == "/events":
		s.handlePlayerEvents(w, r)
	case r.URL.Path == "/admin":
		s.handleAdmin(w, r)
	case r.URL.Path == "/admin/login":
		s.handleAdminLogin(w, r)
	case r.URL.Path == "/admin/callback":
		s.handleAdminCallback(w, r)
	case r.URL.Path == "/admin/events":
		s.handleAdminEvents(w, r)
	case r.URL.Path == "/admin/select-playlist":
		s.handleSelectPlaylist(w, r)
	case r.URL.Path == "/admin/select-device":
		s.handleSelectDevice(w, r)
	case r.URL.Path == "/admin/start-round":
		s.handleStartRound(w, r)
	case r.URL.Path == "/admin/end-round":
		s.handleEndRound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// --- player side ---

const playerCookie = "songgame_player"

func (s *Server) playerID(r *http.Request) string {
	c, err := r.Cookie(playerCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) handlePlayerRoot(w http.ResponseWriter, r *http.Request) {
	id := s.playerID(r)
	if id == "" {
		s.render(w, "join.html", nil)
		return
	}
	p := s.game.GetPlayer(id)
	if p == nil {
		// cookie but server restart lost them; re-join
		s.render(w, "join.html", nil)
		return
	}
	view := s.game.PlayerView(id)
	s.render(w, "player.html", view)
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if len(name) > 40 {
		name = name[:40]
	}
	id := s.playerID(r)
	if id == "" {
		id = randomHex(12)
		http.SetCookie(w, &http.Cookie{
			Name:     playerCookie,
			Value:    id,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   60 * 60 * 24 * 7,
		})
	}
	s.game.AddOrUpdatePlayer(id, name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := s.playerID(r)
	if id == "" || s.game.GetPlayer(id) == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	song := strings.TrimSpace(r.FormValue("song"))
	artist := strings.TrimSpace(r.FormValue("artist"))
	s.game.SubmitAnswer(id, song, artist)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handlePlayerEvents(w http.ResponseWriter, r *http.Request) {
	id := s.playerID(r)
	if id == "" || s.game.GetPlayer(id) == nil {
		http.Error(w, "not joined", http.StatusUnauthorized)
		return
	}
	s.streamEvents(w, r, func() any { return s.game.PlayerView(id) }, "p:"+id, true)
}

// --- admin side ---

const adminCookie = "songgame_admin"

func (s *Server) isAdmin(r *http.Request) bool {
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.Value == s.adminToken
}

// On first admin visit from any browser, auto-set the admin cookie. Since the
// server is run locally for a family game, whoever reaches /admin first owns
// the session. To rotate, restart the server.
func (s *Server) ensureAdmin(w http.ResponseWriter, r *http.Request) {
	if s.isAdmin(r) {
		return
	}
	s.mu.Lock()
	token := s.adminToken
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24,
	})
}

type adminPageData struct {
	Authorized bool
	RedirectURI string
	Playlists  []SpotifyPlaylist
	Devices    []SpotifyDevice
	View       AdminView
	Err        string
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	s.ensureAdmin(w, r)
	data := adminPageData{
		Authorized:  s.spotify.Authorized(),
		RedirectURI: s.cfg.RedirectURI,
		View:        s.game.AdminView(),
	}
	data.View.Authorized = data.Authorized
	if data.Authorized {
		if pls, err := s.spotify.ListPlaylists(); err == nil {
			data.Playlists = pls
		} else {
			data.Err = "playlists: " + err.Error()
		}
		if devs, err := s.spotify.Devices(); err == nil {
			data.Devices = devs
		} else if data.Err == "" {
			data.Err = "devices: " + err.Error()
		}
	}
	s.render(w, "admin.html", data)
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	s.ensureAdmin(w, r)
	state := randomHex(16)
	s.mu.Lock()
	s.oauthState = state
	s.mu.Unlock()
	http.Redirect(w, r, s.spotify.AuthURL(state), http.StatusSeeOther)
}

func (s *Server) handleAdminCallback(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "spotify auth error: "+e, http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	s.mu.Lock()
	expected := s.oauthState
	s.mu.Unlock()
	if state == "" || state != expected {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	if err := s.spotify.ExchangeCode(code); err != nil {
		http.Error(w, "token exchange: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleSelectPlaylist(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.FormValue("playlist_id")
	name := r.FormValue("playlist_name")
	if id == "" {
		http.Error(w, "playlist_id required", http.StatusBadRequest)
		return
	}
	tracks, err := s.spotify.PlaylistTracks(id)
	if err != nil {
		http.Error(w, "load playlist: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(tracks) == 0 {
		http.Error(w, "playlist has no playable tracks", http.StatusBadRequest)
		return
	}
	s.game.SetPlaylist(id, name, tracks)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleSelectDevice(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.FormValue("device_id")
	name := r.FormValue("device_name")
	s.game.SetDevice(id, name)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleStartRound(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	track, err := s.game.StartRound()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// fetch device id from game
	view := s.game.AdminView()
	if err := s.spotify.PlayTrack(view.DeviceID, track.URI); err != nil {
		// don't fail the round — admin can retry playback, but log it
		log.Printf("spotify play error: %v", err)
		http.Error(w, "round started but Spotify play failed: "+err.Error()+"\n(pick a device and make sure Spotify is running)", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleEndRound(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.game.EndRound()
	view := s.game.AdminView()
	if err := s.spotify.Pause(view.DeviceID); err != nil {
		log.Printf("spotify pause error: %v", err)
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.streamEvents(w, r, func() any { return s.game.AdminView() }, "admin", false)
}

// --- SSE helper ---

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, snapshot func() any, subKey string, isPlayer bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	key := subKey + ":" + randomHex(4)
	var ch chan struct{}
	if isPlayer {
		ch = s.game.SubscribePlayer(key)
		defer s.game.UnsubscribePlayer(key)
	} else {
		ch = s.game.SubscribeAdmin(key)
		defer s.game.UnsubscribeAdmin(key)
	}

	send := func() {
		b, _ := json.Marshal(snapshot())
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	send()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			send()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// --- misc ---

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
