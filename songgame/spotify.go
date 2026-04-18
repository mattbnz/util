package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const spotifyScopes = "user-read-playback-state user-modify-playback-state user-read-currently-playing playlist-read-private playlist-read-collaborative"

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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("spotify %s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type SpotifyPlaylist struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"owner"`
	Collaborative bool `json:"collaborative"`
	Tracks        struct {
		Total int `json:"total"`
	} `json:"tracks"`
}

func (s *SpotifyClient) Me() (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := s.do("GET", "/me", nil, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ListPlaylists returns playlists the user owns or collaborates on. Playlists
// owned by Spotify (editorial/algorithmic, like Daily Mixes or Discover Weekly)
// are excluded because Dev Mode apps can't read their contents as of Feb 2026.
func (s *SpotifyClient) ListPlaylists() ([]SpotifyPlaylist, error) {
	me, err := s.Me()
	if err != nil {
		return nil, err
	}
	var all []SpotifyPlaylist
	next := "/me/playlists?limit=50"
	for next != "" {
		var page struct {
			Items []SpotifyPlaylist `json:"items"`
			Next  string            `json:"next"`
		}
		if err := s.do("GET", next, nil, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Items {
			if p.Owner.ID == me || p.Collaborative {
				all = append(all, p)
			}
		}
		if page.Next == "" {
			break
		}
		next = strings.TrimPrefix(page.Next, "https://api.spotify.com/v1")
	}
	return all, nil
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

func (s *SpotifyClient) PlaylistTracks(id string) ([]SpotifyTrack, error) {
	var out []SpotifyTrack
	next := "/playlists/" + id + "/tracks?limit=100&fields=next,items(track(id,uri,name,artists(name),type))"
	for next != "" {
		var page struct {
			Items []struct {
				Track *struct {
					SpotifyTrack
					Type string `json:"type"`
				} `json:"track"`
			} `json:"items"`
			Next string `json:"next"`
		}
		if err := s.do("GET", next, nil, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if it.Track == nil || it.Track.Type != "track" || it.Track.ID == "" {
				continue
			}
			out = append(out, it.Track.SpotifyTrack)
		}
		if page.Next == "" {
			break
		}
		next = strings.TrimPrefix(page.Next, "https://api.spotify.com/v1")
	}
	return out, nil
}

type SpotifyDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	IsActive bool   `json:"is_active"`
}

func (s *SpotifyClient) Devices() ([]SpotifyDevice, error) {
	var out struct {
		Devices []SpotifyDevice `json:"devices"`
	}
	if err := s.do("GET", "/me/player/devices", nil, &out); err != nil {
		return nil, err
	}
	return out.Devices, nil
}

func (s *SpotifyClient) PlayTrack(deviceID, trackURI string) error {
	path := "/me/player/play"
	if deviceID != "" {
		path += "?device_id=" + url.QueryEscape(deviceID)
	}
	body := map[string]any{"uris": []string{trackURI}}
	return s.do("PUT", path, body, nil)
}

func (s *SpotifyClient) Pause(deviceID string) error {
	path := "/me/player/pause"
	if deviceID != "" {
		path += "?device_id=" + url.QueryEscape(deviceID)
	}
	err := s.do("PUT", path, nil, nil)
	// 403 "Player command failed: Restriction violated" happens when already paused — ignore
	if err != nil && strings.Contains(err.Error(), "403") {
		return nil
	}
	return err
}
