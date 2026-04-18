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

type Server struct {
	cfg     ServerConfig
	spotify *SpotifyClient
	game    *Game
	tpl     *template.Template

	mu                sync.Mutex
	oauthState        string
	adminToken        string
	selectedDeviceID  string
	onSelectionChange func()

	// Shared playback-state cache. A single background poller fills this in
	// adaptively; both /admin/spotify-status and the auto-resync logic read
	// from here, so we don't hammer /me/player/currently-playing with
	// concurrent callers.
	cacheMu    sync.RWMutex
	cache      playbackCache
	wakePoller chan struct{}

	// Devices list cache — refreshed lazily, since the device list is fairly
	// static once the host has things set up. See devicesCacheTTL.
	devicesMu      sync.Mutex
	devicesCache   []SpotifyDevice
	devicesErr     error
	devicesPolled  time.Time
}

type playbackCache struct {
	status   *CurrentlyPlaying
	err      error
	polledAt time.Time
}

// How long a Devices() result stays fresh before the next /admin/spotify-status
// call triggers a re-fetch. The Resync action and explicit device refresh
// button bypass this cache.
const devicesCacheTTL = 5 * time.Minute

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
	s.wakePoller = make(chan struct{}, 1)
	s.game.SetCallbacks(nil, s.onAutoAdvance)
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
	snap.SpotifyClientID = s.cfg.ClientID
	snap.SpotifyClientSecret = s.cfg.ClientSecret
	snap.SpotifyDeviceID = s.selectedDeviceID
	s.mu.Unlock()
	snap.SpotifyRefreshToken = s.spotify.RefreshToken()
	return snap
}

// RestoreState applies a previously-saved snapshot at startup. If the
// snapshot carried an admin token or Spotify refresh token, the values
// generated in NewServer are replaced.
func (s *Server) RestoreState(snap StateSnapshot) {
	s.game.RestoreState(snap)
	s.mu.Lock()
	if snap.AdminToken != "" {
		s.adminToken = snap.AdminToken
	}
	s.selectedDeviceID = snap.SpotifyDeviceID
	s.mu.Unlock()
	s.spotify.LoadRefreshToken(snap.SpotifyRefreshToken)
}

// SelectedDeviceID returns the Spotify Connect device the host has chosen to
// target, or "" if no preference (use whatever is currently active).
func (s *Server) SelectedDeviceID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selectedDeviceID
}

// SetServerChangeCallback registers a hook that fires when server-level state
// (currently just the selected Spotify device) changes, so the persistent
// store can mark itself dirty.
func (s *Server) SetServerChangeCallback(fn func()) {
	s.mu.Lock()
	s.onSelectionChange = fn
	s.mu.Unlock()
}

// devicesCached returns the cached device list, fetching from Spotify only
// when the cache is empty/stale or force=true. Caller is not blocked by other
// readers — only by an in-flight fetch on the same call. Returns the polled-at
// timestamp so callers can surface freshness in the UI.
func (s *Server) devicesCached(force bool) ([]SpotifyDevice, time.Time, error) {
	s.devicesMu.Lock()
	defer s.devicesMu.Unlock()
	fresh := !s.devicesPolled.IsZero() && time.Since(s.devicesPolled) < devicesCacheTTL && s.devicesErr == nil
	if !force && fresh {
		return s.devicesCache, s.devicesPolled, s.devicesErr
	}
	devs, err := s.spotify.Devices()
	s.devicesCache = devs
	s.devicesErr = err
	s.devicesPolled = time.Now()
	return s.devicesCache, s.devicesPolled, s.devicesErr
}

func (s *Server) onAutoAdvance() {
	if err := s.advanceToNextRound(); err != nil {
		log.Printf("auto-advance: %v", err)
	}
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
	case "/admin/select-device":
		s.handleSelectDevice(w, r)
	case "/admin/refresh-devices":
		s.handleRefreshDevices(w, r)
	case "/admin/resync":
		s.handleResync(w, r)
	case "/admin/end-game":
		s.handleEndGame(w, r)
	case "/admin/eject":
		s.handleEject(w, r)
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

// Playback poller cadences.
//   - prep: fast polling to confirm the skip / first-round currently-playing
//     as soon as possible; caps at prepTimeout before giving up.
//   - active in sync: slow polling just to detect drift.
//   - idle: long sleep; PokePlaybackPoller wakes the loop when state changes.
const (
	pollPrep    = 700 * time.Millisecond
	pollInSync  = 30 * time.Second
	pollIdle    = 5 * time.Minute
	prepTimeout = 20 * time.Second
)

// PokePlaybackPoller asks the poller to run its next cycle immediately
// (non-blocking). Call this when the expected state changes — a new round
// starts, the admin forces a resync, etc.
func (s *Server) PokePlaybackPoller() {
	select {
	case s.wakePoller <- struct{}{}:
	default:
	}
}

// LastPlaybackStatus returns the most recently cached currently-playing
// response along with the time of the poll and any error recorded.
func (s *Server) LastPlaybackStatus() (*CurrentlyPlaying, time.Time, error) {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return s.cache.status, s.cache.polledAt, s.cache.err
}

// RunPlaybackPoller is the single source of /me/player/currently-playing
// traffic. It caches the last status for the admin UI to read, drives the
// prep→active transition when a round is being set up, and fires immediate
// resyncs when an active round drifts from what Spotify is playing. Cadence
// adapts to the current phase (see pollPrep / pollInSync / pollIdle constants).
//
// Blocks until stop is closed.
func (s *Server) RunPlaybackPoller(stop <-chan struct{}) {
	interval := pollInSync
	for {
		select {
		case <-stop:
			return
		case <-time.After(interval):
		case <-s.wakePoller:
		}
		if !s.spotify.Authorized() {
			interval = pollIdle
			continue
		}
		phase := s.game.RoundPhase()
		if phase == "" || phase == PhaseEnded {
			interval = pollIdle
			continue
		}
		cp, err := s.spotify.CurrentlyPlaying()
		s.cacheMu.Lock()
		s.cache = playbackCache{status: cp, err: err, polledAt: time.Now()}
		s.cacheMu.Unlock()
		if err != nil {
			// transient network / API hiccup — back off briefly and retry
			interval = pollPrep
			continue
		}
		// If a device is selected, ignore reports that come from a different
		// device. Multi-device setups (phone driving a stereo via Connect)
		// otherwise produce confusing prep timeouts and false resyncs.
		if devID := s.SelectedDeviceID(); devID != "" && cp != nil && cp.Device != nil && cp.Device.ID != "" && cp.Device.ID != devID {
			cp = nil
		}
		switch phase {
		case PhasePrep:
			interval = pollPrep
			if cp == nil || cp.Item == nil || cp.Item.URI == "" {
				// nothing playing yet — if we've been trying too long, give up
				if s.prepTimedOut() {
					s.game.CancelPrepRound()
					log.Printf("prep: gave up waiting for Spotify to report a track")
					interval = pollIdle
				}
				continue
			}
			prev := s.game.PrepPrevURI()
			if prev != "" && cp.Item.URI == prev {
				// skip hasn't taken effect yet; keep waiting
				if s.prepTimedOut() {
					s.game.CancelPrepRound()
					log.Printf("prep: gave up — skip didn't take effect within %s", prepTimeout)
					interval = pollIdle
				}
				continue
			}
			// Got a fresh track — activate.
			track := *cp.Item
			s.game.ActivateRound(track)
			log.Printf("prep→active: %q by %v", track.Name, track.ArtistNames())
			interval = pollInSync
		case PhaseActive:
			interval = pollInSync
			expected := s.game.CurrentTrackURI()
			if cp == nil || cp.Item == nil || cp.Item.URI == "" || cp.Item.URI == expected {
				continue
			}
			// Immediate resync — user asked for no delay.
			track := *cp.Item
			if s.game.UpdateRoundTrack(track) {
				log.Printf("auto-resync: round track updated to %q by %v", track.Name, track.ArtistNames())
			}
		}
	}
}

// prepTimedOut reports whether the current prep round has been waiting for
// Spotify longer than prepTimeout. Returns false if no prep round is active.
func (s *Server) prepTimedOut() bool {
	s.game.mu.Lock()
	defer s.game.mu.Unlock()
	if s.game.Round == nil || s.game.Round.Phase != PhasePrep {
		return false
	}
	return time.Since(s.game.Round.PrepStartedAt) > prepTimeout
}

func (s *Server) handleSpotifyStatus(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	type statusOut struct {
		IsPlaying    bool     `json:"is_playing"`
		DeviceID     string   `json:"device_id,omitempty"`
		DeviceName   string   `json:"device_name,omitempty"`
		DeviceType   string   `json:"device_type,omitempty"`
		TrackURI     string   `json:"track_uri,omitempty"`
		TrackName    string   `json:"track_name,omitempty"`
		TrackArtists []string `json:"track_artists,omitempty"`
	}
	type deviceOut struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		IsActive bool   `json:"is_active"`
		Selected bool   `json:"selected"`
	}
	type resp struct {
		Status           *statusOut  `json:"status"`
		Error            string      `json:"error,omitempty"`
		PolledAt         int64       `json:"polled_at,omitempty"`
		AgeSeconds       int         `json:"age_seconds,omitempty"`
		RoundActive      bool        `json:"round_active"`
		RoundTrackURI    string      `json:"round_track_uri"`
		RoundTrackName   string      `json:"round_track_name"`
		Mismatch         bool        `json:"mismatch"`
		Devices           []deviceOut `json:"devices,omitempty"`
		DevicesError      string      `json:"devices_error,omitempty"`
		DevicesAgeSeconds *int        `json:"devices_age_seconds"`
		SelectedDeviceID  string      `json:"selected_device_id,omitempty"`
	}
	cp, at, err := s.LastPlaybackStatus()
	out := resp{SelectedDeviceID: s.SelectedDeviceID()}
	if cp != nil {
		st := &statusOut{IsPlaying: cp.IsPlaying}
		if cp.Device != nil {
			st.DeviceID = cp.Device.ID
			st.DeviceName = cp.Device.Name
			st.DeviceType = cp.Device.Type
		}
		if cp.Item != nil {
			st.TrackURI = cp.Item.URI
			st.TrackName = cp.Item.Name
			st.TrackArtists = cp.Item.ArtistNames()
		}
		out.Status = st
	}
	if !at.IsZero() {
		out.PolledAt = at.Unix()
		out.AgeSeconds = int(time.Since(at).Seconds())
	}
	if err != nil {
		out.Error = err.Error()
	}
	if s.spotify.Authorized() {
		devs, devsAt, derr := s.devicesCached(false)
		if derr != nil {
			out.DevicesError = derr.Error()
		} else {
			out.Devices = make([]deviceOut, 0, len(devs))
			for _, d := range devs {
				out.Devices = append(out.Devices, deviceOut{
					ID: d.ID, Name: d.Name, Type: d.Type, IsActive: d.IsActive,
					Selected: d.ID != "" && d.ID == out.SelectedDeviceID,
				})
			}
		}
		if !devsAt.IsZero() {
			age := int(time.Since(devsAt).Seconds())
			out.DevicesAgeSeconds = &age
		}
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

// handleRefreshDevices forces a re-fetch of the Spotify device list, bypassing
// the TTL cache. Useful when the host just opened Spotify on a new device and
// doesn't want to wait for the next refresh interval.
func (s *Server) handleRefreshDevices(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if _, _, err := s.devicesCached(true); err != nil {
		s.adminError(w, r, "Couldn't list devices: "+err.Error())
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleSelectDevice updates the targeted Spotify Connect device. Empty
// device_id clears the preference (use whatever is currently active).
func (s *Server) handleSelectDevice(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := strings.TrimSpace(r.FormValue("device_id"))
	s.mu.Lock()
	changed := s.selectedDeviceID != id
	s.selectedDeviceID = id
	cb := s.onSelectionChange
	s.mu.Unlock()
	if changed {
		log.Printf("admin: selected Spotify device %q", id)
		if cb != nil {
			cb()
		}
		// re-poke the poller in case the round was waiting on the wrong device
		s.PokePlaybackPoller()
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
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
	if devID := s.SelectedDeviceID(); devID != "" && cp.Device != nil && cp.Device.ID != "" && cp.Device.ID != devID {
		s.adminError(w, r, "Resync failed: Spotify is playing on a different device than the one you selected.")
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
	// Resync is also the natural moment to refresh the device list — the host
	// is already telling us "things might have changed, look again".
	if _, _, err := s.devicesCached(true); err != nil {
		log.Printf("resync: device refresh failed: %v", err)
	}
	s.PokePlaybackPoller()
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleEndGame(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.game.EndGame()
	log.Printf("admin ended the current game; archived to history")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleEject(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) || r.Method != http.MethodPost {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.FormValue("player_id")
	if id == "" {
		http.Error(w, "player_id required", http.StatusBadRequest)
		return
	}
	if s.game.EjectPlayer(id) {
		log.Printf("admin ejected player %s", id)
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
	s.game.EndRound()
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// advanceToNextRound begins the next round. It issues a skip only if a
// previous round exists (round 1 keeps whatever is playing) and opens a
// prep-phase round. The playback poller then watches
// /me/player/currently-playing until the track URI differs from the prev
// round's (or any track appears for round 1) and flips the round to active.
// Players don't see the round until that transition happens. For round 1 the
// host is expected to already have Spotify playing on a device.
func (s *Server) advanceToNextRound() error {
	prevURI := s.game.CurrentTrackURI() // non-empty only if a completed round is still around
	if prevURI != "" {
		if err := s.spotify.Next(); err != nil {
			return fmt.Errorf("couldn't skip to the next song: %v — make sure Spotify is open and playing on a device", err)
		}
	}
	s.game.BeginPrepRound(prevURI)
	s.PokePlaybackPoller()
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

