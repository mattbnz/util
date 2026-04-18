package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const spotifyScopes = "user-read-playback-state user-modify-playback-state user-read-currently-playing"

type SpotifyClient struct {
	clientID     string
	clientSecret string
	redirectURI  string

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
}

func NewSpotifyClient(id, secret, redirect string) *SpotifyClient {
	return &SpotifyClient{clientID: id, clientSecret: secret, redirectURI: redirect}
}

func (s *SpotifyClient) Authorized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshToken != ""
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
	s.mu.Unlock()
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
	s.mu.Unlock()
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

// PlaybackStatus is a richer snapshot of /me/player, exposed to the admin UI
// so the host can debug device/state mismatches.
type PlaybackStatus struct {
	DeviceID      string   `json:"device_id"`
	DeviceName    string   `json:"device_name"`
	DeviceType    string   `json:"device_type"`
	VolumePercent int      `json:"volume_percent"`
	IsActive      bool     `json:"is_active"`
	IsPlaying     bool     `json:"is_playing"`
	Shuffle       bool     `json:"shuffle"`
	RepeatState   string   `json:"repeat_state"`
	TrackURI      string   `json:"track_uri"`
	TrackName     string   `json:"track_name"`
	TrackArtists  []string `json:"track_artists"`
	ProgressMS    int      `json:"progress_ms"`
	DurationMS    int      `json:"duration_ms"`
	Raw204        bool     `json:"raw_204"` // true when Spotify returned "no playback session" (204)
}

// PlaybackStatus returns a richer view of the current Spotify player state.
func (s *SpotifyClient) PlaybackStatus() (*PlaybackStatus, error) {
	if err := s.refreshIfNeeded(); err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("GET", "https://api.spotify.com/v1/me/player", nil)
	s.mu.Lock()
	req.Header.Set("Authorization", "Bearer "+s.accessToken)
	s.mu.Unlock()
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logCall("GET", "/me/player", 0, time.Since(start), err.Error())
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	logCall("GET", "/me/player", resp.StatusCode, time.Since(start), string(data))
	if resp.StatusCode == 204 {
		return &PlaybackStatus{Raw204: true}, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("player: %d %s", resp.StatusCode, data)
	}
	var raw struct {
		Device *struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Type          string `json:"type"`
			VolumePercent int    `json:"volume_percent"`
			IsActive      bool   `json:"is_active"`
		} `json:"device"`
		ShuffleState bool   `json:"shuffle_state"`
		RepeatState  string `json:"repeat_state"`
		IsPlaying    bool   `json:"is_playing"`
		ProgressMS   int    `json:"progress_ms"`
		Item         *struct {
			URI        string `json:"uri"`
			Name       string `json:"name"`
			DurationMS int    `json:"duration_ms"`
			Artists    []struct {
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	st := &PlaybackStatus{
		IsPlaying:   raw.IsPlaying,
		Shuffle:     raw.ShuffleState,
		RepeatState: raw.RepeatState,
		ProgressMS:  raw.ProgressMS,
	}
	if raw.Device != nil {
		st.DeviceID = raw.Device.ID
		st.DeviceName = raw.Device.Name
		st.DeviceType = raw.Device.Type
		st.VolumePercent = raw.Device.VolumePercent
		st.IsActive = raw.Device.IsActive
	}
	if raw.Item != nil {
		st.TrackURI = raw.Item.URI
		st.TrackName = raw.Item.Name
		st.DurationMS = raw.Item.DurationMS
		for _, a := range raw.Item.Artists {
			st.TrackArtists = append(st.TrackArtists, a.Name)
		}
	}
	return st, nil
}

// Next skips to the next track on whichever device is currently active.
func (s *SpotifyClient) Next() error {
	return s.do("POST", "/me/player/next", nil, nil)
}

// Play resumes playback on whichever device is currently active.
func (s *SpotifyClient) Play() error {
	err := s.do("PUT", "/me/player/play", nil, nil)
	// 403 "Restriction violated" can mean already playing — benign
	if err != nil && strings.Contains(err.Error(), "403") {
		return nil
	}
	return err
}

// Pause pauses playback on whichever device is currently active.
func (s *SpotifyClient) Pause() error {
	err := s.do("PUT", "/me/player/pause", nil, nil)
	// 403 typically means already paused — benign
	if err != nil && strings.Contains(err.Error(), "403") {
		return nil
	}
	return err
}

// DeviceVolume returns the active device's volume (0-100), or -1 if unknown.
func (s *SpotifyClient) DeviceVolume() (int, error) {
	st, err := s.PlaybackStatus()
	if err != nil {
		return -1, err
	}
	if st == nil || st.Raw204 || st.DeviceID == "" {
		return -1, nil
	}
	return st.VolumePercent, nil
}

// SetVolume sets the active device's volume (0-100). Not all devices support
// this — Bluetooth speakers in particular may reject it.
func (s *SpotifyClient) SetVolume(pct int) error {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return s.do("PUT", "/me/player/volume?volume_percent="+strconv.Itoa(pct), nil, nil)
}
