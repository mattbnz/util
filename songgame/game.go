package main

import (
	"math/rand"
	"sort"
	"sync"
	"time"
)

type Player struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Score    int       `json:"score"`
	JoinedAt time.Time `json:"-"`
}

type Answer struct {
	PlayerID      string    `json:"player_id"`
	PlayerName    string    `json:"player_name"`
	SongGuess     string    `json:"song_guess"`
	ArtistGuess   string    `json:"artist_guess"`
	SongCorrect   bool      `json:"song_correct"`
	ArtistCorrect bool      `json:"artist_correct"`
	SubmittedAt   time.Time `json:"-"`
}

type Round struct {
	Number    int                `json:"number"`
	Track     SpotifyTrack       `json:"-"`
	Answers   map[string]*Answer `json:"-"`
	StartedAt time.Time          `json:"-"`
	Ended     bool               `json:"ended"`
	EndedAt   time.Time          `json:"-"`
}

type Game struct {
	mu sync.Mutex

	PlaylistID   string
	PlaylistName string
	DeviceID     string
	DeviceName   string

	Tracks     []SpotifyTrack
	TrackQueue []int

	Round   *Round
	Players map[string]*Player
	Number  int

	playerSubs sync.Map // id -> chan struct{} (notify)
	adminSubs  sync.Map

	rng *rand.Rand
}

func NewGame() *Game {
	return &Game{
		Players: make(map[string]*Player),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// --- subscription plumbing ---

func (g *Game) SubscribePlayer(id string) chan struct{} {
	ch := make(chan struct{}, 4)
	g.playerSubs.Store(id, ch)
	return ch
}
func (g *Game) UnsubscribePlayer(id string)   { g.playerSubs.Delete(id) }
func (g *Game) SubscribeAdmin(id string) chan struct{} {
	ch := make(chan struct{}, 4)
	g.adminSubs.Store(id, ch)
	return ch
}
func (g *Game) UnsubscribeAdmin(id string) { g.adminSubs.Delete(id) }

func (g *Game) notify() {
	g.playerSubs.Range(func(_, v any) bool {
		select {
		case v.(chan struct{}) <- struct{}{}:
		default:
		}
		return true
	})
	g.adminSubs.Range(func(_, v any) bool {
		select {
		case v.(chan struct{}) <- struct{}{}:
		default:
		}
		return true
	})
}

// --- player management ---

func (g *Game) AddOrUpdatePlayer(id, name string) *Player {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.Players[id]; ok {
		if name != "" {
			p.Name = name
		}
		go g.notify()
		return p
	}
	p := &Player{ID: id, Name: name, JoinedAt: time.Now()}
	g.Players[id] = p
	go g.notify()
	return p
}

func (g *Game) GetPlayer(id string) *Player {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Players[id]
}

func (g *Game) PlayerList() []*Player {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*Player, 0, len(g.Players))
	for _, p := range g.Players {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].JoinedAt.Before(out[j].JoinedAt)
	})
	return out
}

// --- playlist setup ---

func (g *Game) SetPlaylist(id, name string, tracks []SpotifyTrack) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.PlaylistID = id
	g.PlaylistName = name
	g.Tracks = tracks
	g.TrackQueue = g.rng.Perm(len(tracks))
	g.Round = nil
	g.Number = 0
	go g.notify()
}

func (g *Game) SetDevice(id, name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.DeviceID = id
	g.DeviceName = name
	go g.notify()
}

// --- round management ---

// StartRound picks the next track and returns it; caller is responsible for telling Spotify to play it.
func (g *Game) StartRound() (*SpotifyTrack, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.Tracks) == 0 {
		return nil, errStr("no playlist loaded")
	}
	if len(g.TrackQueue) == 0 {
		g.TrackQueue = g.rng.Perm(len(g.Tracks))
	}
	idx := g.TrackQueue[0]
	g.TrackQueue = g.TrackQueue[1:]
	track := g.Tracks[idx]
	g.Number++
	g.Round = &Round{
		Number:    g.Number,
		Track:     track,
		Answers:   make(map[string]*Answer),
		StartedAt: time.Now(),
	}
	go g.notify()
	return &track, nil
}

// SubmitAnswer records a player's guess for the active round.
// Returns (accepted, songCorrect, artistCorrect, roundClosed).
func (g *Game) SubmitAnswer(playerID, songGuess, artistGuess string) (bool, bool, bool, bool) {
	g.mu.Lock()
	if g.Round == nil || g.Round.Ended {
		g.mu.Unlock()
		return false, false, false, false
	}
	p, ok := g.Players[playerID]
	if !ok {
		g.mu.Unlock()
		return false, false, false, false
	}
	if _, already := g.Round.Answers[playerID]; already {
		g.mu.Unlock()
		return false, false, false, false
	}
	songOK := fuzzyMatch(songGuess, g.Round.Track.Name)
	artistOK := matchAnyArtist(artistGuess, g.Round.Track.ArtistNames())
	ans := &Answer{
		PlayerID:      playerID,
		PlayerName:    p.Name,
		SongGuess:     songGuess,
		ArtistGuess:   artistGuess,
		SongCorrect:   songOK,
		ArtistCorrect: artistOK,
		SubmittedAt:   time.Now(),
	}
	g.Round.Answers[playerID] = ans
	// Check auto-end: once half or more of players have answered
	closed := false
	if len(g.Round.Answers)*2 >= len(g.Players) && len(g.Players) > 0 {
		g.endRoundLocked()
		closed = true
	}
	g.mu.Unlock()
	go g.notify()
	return true, songOK, artistOK, closed
}

// EndRound ends the current round immediately. Returns whether a round was ended.
func (g *Game) EndRound() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Round == nil || g.Round.Ended {
		return false
	}
	g.endRoundLocked()
	go g.notify()
	return true
}

// must be called with g.mu held
func (g *Game) endRoundLocked() {
	g.Round.Ended = true
	g.Round.EndedAt = time.Now()
	for _, ans := range g.Round.Answers {
		p := g.Players[ans.PlayerID]
		if p == nil {
			continue
		}
		if ans.SongCorrect {
			p.Score++
		}
		if ans.ArtistCorrect {
			p.Score++
		}
	}
}

// --- state snapshots for rendering/SSE ---

type PlayerView struct {
	PlaylistName   string        `json:"playlist_name"`
	RoundNumber    int           `json:"round_number"`
	RoundActive    bool          `json:"round_active"`
	RoundEnded     bool          `json:"round_ended"`
	HasAnswered    bool          `json:"has_answered"`
	YourSongOK     bool          `json:"your_song_ok"`
	YourArtistOK   bool          `json:"your_artist_ok"`
	YourSongGuess  string        `json:"your_song_guess"`
	YourArtistGuess string       `json:"your_artist_guess"`
	AnswerCount    int           `json:"answer_count"`
	PlayerCount    int           `json:"player_count"`
	RevealSong     string        `json:"reveal_song,omitempty"`
	RevealArtists  string        `json:"reveal_artists,omitempty"`
	Scoreboard     []*Player     `json:"scoreboard"`
	You            *Player       `json:"you"`
}

func (g *Game) PlayerView(playerID string) PlayerView {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := PlayerView{
		PlaylistName: g.PlaylistName,
		PlayerCount:  len(g.Players),
	}
	if p, ok := g.Players[playerID]; ok {
		v.You = p
	}
	if g.Round != nil {
		v.RoundNumber = g.Round.Number
		v.RoundActive = !g.Round.Ended
		v.RoundEnded = g.Round.Ended
		v.AnswerCount = len(g.Round.Answers)
		if ans, ok := g.Round.Answers[playerID]; ok {
			v.HasAnswered = true
			v.YourSongGuess = ans.SongGuess
			v.YourArtistGuess = ans.ArtistGuess
			// Don't reveal correctness until round ends
			if g.Round.Ended {
				v.YourSongOK = ans.SongCorrect
				v.YourArtistOK = ans.ArtistCorrect
			}
		}
		if g.Round.Ended {
			v.RevealSong = g.Round.Track.Name
			v.RevealArtists = joinArtists(g.Round.Track.ArtistNames())
		}
	}
	v.Scoreboard = g.playerListLocked()
	return v
}

type AdminView struct {
	Authorized     bool              `json:"authorized"`
	PlaylistName   string            `json:"playlist_name"`
	PlaylistID     string            `json:"playlist_id"`
	DeviceID       string            `json:"device_id"`
	DeviceName     string            `json:"device_name"`
	TrackCount     int               `json:"track_count"`
	RoundNumber    int               `json:"round_number"`
	RoundActive    bool              `json:"round_active"`
	RoundEnded     bool              `json:"round_ended"`
	CurrentSong    string            `json:"current_song"`
	CurrentArtists string            `json:"current_artists"`
	AnswerCount    int               `json:"answer_count"`
	PlayerCount    int               `json:"player_count"`
	Answers        []*Answer         `json:"answers"`
	Scoreboard     []*Player         `json:"scoreboard"`
}

func (g *Game) AdminView() AdminView {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := AdminView{
		PlaylistName: g.PlaylistName,
		PlaylistID:   g.PlaylistID,
		DeviceID:     g.DeviceID,
		DeviceName:   g.DeviceName,
		TrackCount:   len(g.Tracks),
		PlayerCount:  len(g.Players),
	}
	if g.Round != nil {
		v.RoundNumber = g.Round.Number
		v.RoundActive = !g.Round.Ended
		v.RoundEnded = g.Round.Ended
		v.CurrentSong = g.Round.Track.Name
		v.CurrentArtists = joinArtists(g.Round.Track.ArtistNames())
		v.AnswerCount = len(g.Round.Answers)
		v.Answers = make([]*Answer, 0, len(g.Round.Answers))
		for _, a := range g.Round.Answers {
			v.Answers = append(v.Answers, a)
		}
		sort.Slice(v.Answers, func(i, j int) bool {
			return v.Answers[i].SubmittedAt.Before(v.Answers[j].SubmittedAt)
		})
	}
	v.Scoreboard = g.playerListLocked()
	return v
}

func (g *Game) playerListLocked() []*Player {
	out := make([]*Player, 0, len(g.Players))
	for _, p := range g.Players {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].JoinedAt.Before(out[j].JoinedAt)
	})
	return out
}

func joinArtists(artists []string) string {
	if len(artists) == 0 {
		return ""
	}
	if len(artists) == 1 {
		return artists[0]
	}
	s := artists[0]
	for i := 1; i < len(artists)-1; i++ {
		s += ", " + artists[i]
	}
	s += " & " + artists[len(artists)-1]
	return s
}

type errStr string

func (e errStr) Error() string { return string(e) }
