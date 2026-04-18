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
	"net/url"
	"strconv"
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
	BaseURL      string // e.g. http://127.0.0.1:8080 — used to print the shareable admin URL
}

// Volume to drop to during the results period. 30% is a "background" level
// on most devices — still audible, but quiet enough to talk over.
const duckedPercent = 30

type Server struct {
	cfg     ServerConfig
	spotify *SpotifyClient
	game    *Game
	tpl     *template.Template

	mu         sync.Mutex
	oauthState string
	adminToken string

	volMu          sync.Mutex
	preDuckVolume  int // 0 = not ducked; otherwise the volume we should restore to
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
	s.game.SetCallbacks(s.onRoundEnd, s.onAutoAdvance)
	return s
}

// Game exposes the underlying game for wiring up storage/callbacks at startup.
func (s *Server) Game() *Game { return s.game }

// AdminToken returns the current admin token (for tests and logging).
func (s *Server) AdminToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adminToken
}

// Spotify exposes the Spotify client so main.go can wire up the token-change
// callback for persistence.
func (s *Server) Spotify() *SpotifyClient { return s.spotify }

// AdminURL returns the shareable URL that claims admin access.
func (s *Server) AdminURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.BaseURL + "/admin?t=" + s.adminToken
}

// Snapshot captures everything that should survive a restart: game state,
// the admin token, and Spotify's refresh token (the access token is
// deliberately not persisted — it'll be refreshed on next use).
func (s *Server) Snapshot() StateSnapshot {
	snap := s.game.Snapshot()
	s.mu.Lock()
	snap.AdminToken = s.adminToken
	s.mu.Unlock()
	snap.SpotifyRefreshToken = s.spotify.RefreshToken()
	return snap
}

// RestoreState applies a previously-saved snapshot at startup. If the
// snapshot carried an admin token or Spotify refresh token, the values
// generated in NewServer are replaced.
func (s *Server) RestoreState(snap StateSnapshot) {
	s.game.RestoreState(snap)
	if snap.AdminToken != "" {
		s.mu.Lock()
		s.adminToken = snap.AdminToken
		s.mu.Unlock()
	}
	s.spotify.LoadRefreshToken(snap.SpotifyRefreshToken)
}

func (s *Server) onRoundEnd() {
	s.duckVolume()
}

func (s *Server) onAutoAdvance() {
	if err := s.advanceToNextRound(); err != nil {
		log.Printf("auto-advance: %v", err)
	}
}

func (s *Server) duckVolume() {
	s.volMu.Lock()
	defer s.volMu.Unlock()
	if s.preDuckVolume > 0 {
		return // already ducked
	}
	vol, err := s.spotify.DeviceVolume()
	if err != nil || vol < 0 {
		return
	}
	if vol <= duckedPercent {
		return // already quiet enough
	}
	if err := s.spotify.SetVolume(duckedPercent); err != nil {
		log.Printf("duck volume: %v", err)
		return
	}
	s.preDuckVolume = vol
}

func (s *Server) unduckVolume() {
	s.volMu.Lock()
	defer s.volMu.Unlock()
	if s.preDuckVolume == 0 {
		return
	}
	if err := s.spotify.SetVolume(s.preDuckVolume); err != nil {
		log.Printf("restore volume: %v", err)
	}
	s.preDuckVolume = 0
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		s.handlePlayerRoot(w, r)
	case "/join":
		s.handleJoin(w, r)
	case "/answer":
		s.handleAnswer(w, r)
	case "/events":
		s.handlePlayerEvents(w, r)
	case "/admin":
		s.handleAdmin(w, r)
	case "/admin/login":
		s.handleAdminLogin(w, r)
	case "/admin/callback":
		s.handleAdminCallback(w, r)
	case "/admin/events":
		s.handleAdminEvents(w, r)
	case "/admin/start-round":
		s.handleStartRound(w, r)
	case "/admin/end-round":
		s.handleEndRound(w, r)
	case "/admin/config":
		s.handleConfig(w, r)
	case "/admin/spotify-status":
		s.handleSpotifyStatus(w, r)
	case "/admin/resync":
		s.handleResync(w, r)
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
	if id == "" || s.game.GetPlayer(id) == nil {
		s.render(w, "join.html", nil)
		return
	}
	s.render(w, "player.html", s.game.PlayerView(id))
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

// claimAdmin sets the admin cookie if the supplied token matches.
// Returns true if the caller is authenticated after this call.
func (s *Server) claimAdmin(w http.ResponseWriter, r *http.Request, token string) bool {
	s.mu.Lock()
	expected := s.adminToken
	s.mu.Unlock()
	if token == "" || token != expected {
		return s.isAdmin(r)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 7,
	})
	return true
}

type adminPageData struct {
	Authorized     bool
	RedirectURI    string
	AdminURL       string
	View           AdminView
	GraceSeconds   int
	ResultsSeconds int
	Err            string
	Flash          string
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	// Claim flow: /admin?t=<token> sets the cookie, then redirects to /admin
	// without the token so it doesn't linger in the address bar or history.
	if tok := r.URL.Query().Get("t"); tok != "" {
		if !s.claimAdmin(w, r, tok) {
			http.Error(w, "invalid admin token", http.StatusForbidden)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if !s.isAdmin(r) {
		s.render(w, "admin_claim.html", map[string]any{})
		return
	}
	grace, results := s.game.Durations()
	data := adminPageData{
		Authorized:     s.spotify.Authorized(),
		RedirectURI:    s.cfg.RedirectURI,
		AdminURL:       s.AdminURL(),
		View:           s.game.AdminView(),
		GraceSeconds:   int(grace / time.Second),
		ResultsSeconds: int(results / time.Second),
		Err:            r.URL.Query().Get("err"),
		Flash:          r.URL.Query().Get("flash"),
	}
	data.View.Authorized = data.Authorized
	s.render(w, "admin.html", data)
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "forbidden — claim admin first", http.StatusForbidden)
		return
	}
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

func (s *Server) handleStartRound(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.game.CancelAutoAdvance()
	if err := s.advanceToNextRound(); err != nil {
		s.adminError(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// RunAutoResync periodically polls Spotify's playback state and, if an active
// round's expected track has drifted from what Spotify is actually playing
// across two consecutive observations, updates the round's track and re-grades
// the submitted answers. Blocks until stop is closed.
func (s *Server) RunAutoResync(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var observedURI string
	var observedCount int
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if !s.spotify.Authorized() || !s.game.HasActiveRound() {
			observedURI = ""
			observedCount = 0
			continue
		}
		expected := s.game.CurrentTrackURI()
		st, err := s.spotify.PlaybackStatus()
		if err != nil {
			continue
		}
		if st == nil || st.Raw204 || st.TrackURI == "" || st.TrackURI == expected {
			observedURI = ""
			observedCount = 0
			continue
		}
		// Mismatch — require persistence across two polls to ride out the
		// brief staleness Spotify reports right after a skip.
		if observedURI == st.TrackURI {
			observedCount++
		} else {
			observedURI = st.TrackURI
			observedCount = 1
		}
		if observedCount < 2 {
			continue
		}
		track, ok := st.AsTrack()
		if !ok {
			observedURI = ""
			observedCount = 0
			continue
		}
		if s.game.UpdateRoundTrack(track) {
			log.Printf("auto-resync: round track updated to %q by %v (was out of sync for 2 polls)",
				track.Name, track.ArtistNames())
		}
		observedURI = ""
		observedCount = 0
	}
}

func (s *Server) handleSpotifyStatus(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	type resp struct {
		Status         *PlaybackStatus `json:"status"`
		Error          string          `json:"error,omitempty"`
		RoundActive    bool            `json:"round_active"`
		RoundTrackURI  string          `json:"round_track_uri"`
		RoundTrackName string          `json:"round_track_name"`
		Mismatch       bool            `json:"mismatch"`
	}
	out := resp{}
	st, err := s.spotify.PlaybackStatus()
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Status = st
	}
	s.game.mu.Lock()
	if s.game.Round != nil && !s.game.Round.Ended {
		out.RoundActive = true
		out.RoundTrackURI = s.game.Round.Track.URI
		out.RoundTrackName = s.game.Round.Track.Name
	}
	s.game.mu.Unlock()
	if out.RoundActive && out.Status != nil && out.Status.TrackURI != "" && out.Status.TrackURI != out.RoundTrackURI {
		out.Mismatch = true
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleResync(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	cp, err := s.spotify.CurrentlyPlaying()
	if err != nil {
		s.adminError(w, r, "Resync failed: couldn't read currently-playing: "+err.Error())
		return
	}
	if cp == nil || cp.Item == nil || cp.Item.URI == "" {
		s.adminError(w, r, "Resync failed: Spotify isn't reporting a track. Start playback first.")
		return
	}
	if !s.game.HasActiveRound() {
		s.adminError(w, r, "Resync needs an active round. Use Start next round for a fresh one.")
		return
	}
	updated := s.game.UpdateRoundTrack(*cp.Item)
	if updated {
		log.Printf("resync: updated round track to %q by %v", cp.Item.Name, cp.Item.ArtistNames())
	} else {
		log.Printf("resync: no change; round track already matches Spotify")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	grace, _ := strconv.Atoi(r.FormValue("grace_seconds"))
	results, _ := strconv.Atoi(r.FormValue("results_seconds"))
	s.game.SetDurations(time.Duration(grace)*time.Second, time.Duration(results)*time.Second)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleEndRound(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.game.EndRound() // callbacks handle ducking playback
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// advanceToNextRound un-ducks volume, skips to the next track (if a previous
// round exists), resumes playback, polls for the new currently-playing track,
// and opens a new round. Shared by the manual start handler and the
// auto-advance timer callback.
func (s *Server) advanceToNextRound() error {
	s.unduckVolume()
	if s.game.HasPreviousRound() {
		if err := s.spotify.Next(); err != nil {
			return fmt.Errorf("couldn't skip to the next song: %v — make sure Spotify is open and playing on a device", err)
		}
	}
	if err := s.spotify.Play(); err != nil {
		return fmt.Errorf("couldn't start playback: %v — open Spotify, start your playlist (shuffle recommended), then try again", err)
	}
	prevURI := s.game.CurrentTrackURI()
	var track *SpotifyTrack
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		cp, err := s.spotify.CurrentlyPlaying()
		if err == nil && cp != nil && cp.Item != nil && cp.Item.URI != "" && cp.Item.URI != prevURI {
			track = cp.Item
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	if track == nil {
		return fmt.Errorf("Spotify didn't report a track playing within a few seconds — make sure a playlist is playing on your phone (or another device) and try again")
	}
	s.game.StartRound(*track)
	return nil
}

func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.streamEvents(w, r, func() any { return s.game.AdminView() }, "admin", false)
}

func (s *Server) adminError(w http.ResponseWriter, r *http.Request, msg string) {
	log.Printf("admin: %s", msg)
	http.Redirect(w, r, "/admin?err="+url.QueryEscape(msg), http.StatusSeeOther)
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

