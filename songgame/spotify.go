package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const spotifyScopes = "user-read-currently-playing user-modify-playback-state"

type SpotifyClient struct {
	clientID     string
	clientSecret string
	redirectURI  string

	mu            sync.Mutex
	accessToken   string
	refreshToken  string
	expiresAt     time.Time
	onTokenChange func()
}

func NewSpotifyClient(id, secret, redirect string) *SpotifyClient {
	return &SpotifyClient{clientID: id, clientSecret: secret, redirectURI: redirect}
}

// SetTokenCallback registers a hook that fires whenever the stored refresh
// token changes (OAuth exchange, token refresh, or clearing on invalid_grant).
// Used by the persistent store to mark itself dirty so the new token is saved.
func (s *SpotifyClient) SetTokenCallback(fn func()) {
	s.mu.Lock()
	s.onTokenChange = fn
	s.mu.Unlock()
}

func (s *SpotifyClient) Authorized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshToken != ""
}

// RefreshToken returns the stored refresh token (for persistence).
func (s *SpotifyClient) RefreshToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshToken
}

// LoadRefreshToken seeds the client with a previously-persisted refresh token.
// The access token and expiry are cleared so the next API call triggers a
// refresh using this refresh token. A no-op if empty.
func (s *SpotifyClient) LoadRefreshToken(rt string) {
	if rt == "" {
		return
	}
	s.mu.Lock()
	s.refreshToken = rt
	s.accessToken = ""
	s.expiresAt = time.Time{}
	s.mu.Unlock()
}

func (s *SpotifyClient) AuthURL(state string) string {
	v := url.Values{}
	v.Set("client_id", s.clientID)
	v.Set("response_type", "code")
	v.Set("redirect_uri", s.redirectURI)
	v.Set("scope", spotifyScopes)
	v.Set("state", state)
	return "https://accounts.spotify.com/authorize?" + v.Encode()
}

func (s *SpotifyClient) ExchangeCode(code string) error {
	v := url.Values{}
	v.Set("grant_type", "authorization_code")
	v.Set("code", code)
	v.Set("redirect_uri", s.redirectURI)

	req, _ := http.NewRequest("POST", "https://accounts.spotify.com/api/token", strings.NewReader(v.Encode()))
	req.SetBasicAuth(s.clientID, s.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, body)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return err
	}
	s.mu.Lock()
	s.accessToken = tok.AccessToken
	s.refreshToken = tok.RefreshToken
	s.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn-30) * time.Second)
	cb := s.onTokenChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

func (s *SpotifyClient) refreshIfNeeded() error {
	s.mu.Lock()
	if s.refreshToken == "" {
		s.mu.Unlock()
		return fmt.Errorf("not authorized")
	}
	if time.Now().Before(s.expiresAt) && s.accessToken != "" {
		s.mu.Unlock()
		return nil
	}
	rt := s.refreshToken
	s.mu.Unlock()

	v := url.Values{}
	v.Set("grant_type", "refresh_token")
	v.Set("refresh_token", rt)
	req, _ := http.NewRequest("POST", "https://accounts.spotify.com/api/token", strings.NewReader(v.Encode()))
	req.SetBasicAuth(s.clientID, s.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// 400 from the refresh endpoint almost always means the refresh token has
	// been revoked/expired. Clear our cached credentials so Authorized()
	// returns false and the admin is prompted to log in again.
	if resp.StatusCode == 400 {
		s.mu.Lock()
		s.accessToken = ""
		s.refreshToken = ""
		s.expiresAt = time.Time{}
		cb := s.onTokenChange
		s.mu.Unlock()
		if cb != nil {
			cb()
		}
		return fmt.Errorf("refresh failed (%d): %s (re-login needed)", resp.StatusCode, body)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("refresh failed (%d): %s", resp.StatusCode, body)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return err
	}
	s.mu.Lock()
	s.accessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		s.refreshToken = tok.RefreshToken
	}
	s.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn-30) * time.Second)
	cb := s.onTokenChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

// logCall writes a single-line trace of a Spotify API call to help debug
// playback / sync issues. Status 0 indicates a network error before a response.
func logCall(method, path string, status int, dur time.Duration, errBody string) {
	d := dur.Round(time.Millisecond)
	if status == 0 {
		log.Printf("spotify %s %s → network error in %v: %s", method, path, d, errBody)
		return
	}
	if status >= 400 {
		trimmed := errBody
		if len(trimmed) > 240 {
			trimmed = trimmed[:240] + "…"
		}
		log.Printf("spotify %s %s → %d in %v: %s", method, path, status, d, trimmed)
		return
	}
	log.Printf("spotify %s %s → %d in %v", method, path, status, d)
}

func (s *SpotifyClient) do(method, path string, body any, out any) error {
	if err := s.refreshIfNeeded(); err != nil {
		return err
	}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.spotify.com/v1"+path, r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	req.Header.Set("Authorization", "Bearer "+s.accessToken)
	s.mu.Unlock()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logCall(method, path, 0, time.Since(start), err.Error())
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	logCall(method, path, resp.StatusCode, time.Since(start), string(data))
	if resp.StatusCode == 204 {
		return nil
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("spotify %s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type SpotifyTrack struct {
	ID      string `json:"id"`
	URI     string `json:"uri"`
	Name    string `json:"name"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
}

func (t SpotifyTrack) ArtistNames() []string {
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	return names
}

type CurrentlyPlaying struct {
	IsPlaying bool          `json:"is_playing"`
	Item      *SpotifyTrack `json:"item"`
	Device    *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"device"`
}

// CurrentlyPlaying returns what Spotify is currently playing, or nil if nothing is active.
func (s *SpotifyClient) CurrentlyPlaying() (*CurrentlyPlaying, error) {
	if err := s.refreshIfNeeded(); err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", "https://api.spotify.com/v1/me/player/currently-playing", nil)
	s.mu.Lock()
	req.Header.Set("Authorization", "Bearer "+s.accessToken)
	s.mu.Unlock()
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logCall("GET", "/me/player/currently-playing", 0, time.Since(start), err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	logCall("GET", "/me/player/currently-playing", resp.StatusCode, time.Since(start), string(data))
	// 204 = nothing playing
	if resp.StatusCode == 204 {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("currently-playing: %d %s", resp.StatusCode, data)
	}
	var cp CurrentlyPlaying
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

type SpotifyDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	IsActive bool   `json:"is_active"`
}

// Devices lists the user's available Spotify Connect devices.
func (s *SpotifyClient) Devices() ([]SpotifyDevice, error) {
	var out struct {
		Devices []SpotifyDevice `json:"devices"`
	}
	if err := s.do("GET", "/me/player/devices", nil, &out); err != nil {
		return nil, err
	}
	return out.Devices, nil
}

// Next skips to the next track on whichever device is currently active.
// We deliberately don't pass device_id: some Spotify Connect endpoints (AV
// receivers, smart speakers) react badly to device-targeted skips — at best
// returning 403 "Restriction violated", at worst halting playback entirely.
// The selected device's job is to filter currently-playing reports, not to
// route control commands.
func (s *SpotifyClient) Next() error {
	return s.do("POST", "/me/player/next", nil, nil)
}
