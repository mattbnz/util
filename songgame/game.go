package main

import (
	"sort"
	"sync"
	"time"
)

// Timings for the round lifecycle. Grace runs after 50% of players answer;
// results period runs after the round ends before auto-advancing.
const (
	graceDuration   = 30 * time.Second
	resultsDuration = 30 * time.Second
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
	Number        int
	Track         SpotifyTrack
	Answers       map[string]*Answer
	StartedAt     time.Time
	GraceUntil    time.Time // non-zero once 50% have answered; round closes at this time
	Ended         bool
	EndedAt       time.Time
	AutoAdvanceAt time.Time // set on end; next round auto-starts at this time
}

type Game struct {
	mu sync.Mutex

	Round   *Round
	Players map[string]*Player
	Number  int

	graceTimer       *time.Timer
	autoAdvanceTimer *time.Timer
	onRoundEnd       func()
	onAutoAdvance    func()

	playerSubs sync.Map
	adminSubs  sync.Map
}

func NewGame() *Game {
	return &Game{Players: make(map[string]*Player)}
}

// SetCallbacks registers hooks for round lifecycle. onRoundEnd runs (in a
// goroutine) immediately after a round ends; onAutoAdvance fires after the
// results period elapses, unless cancelled by a manual start.
func (g *Game) SetCallbacks(onRoundEnd, onAutoAdvance func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onRoundEnd = onRoundEnd
	g.onAutoAdvance = onAutoAdvance
}

// --- subscription plumbing ---

func (g *Game) SubscribePlayer(id string) chan struct{} {
	ch := make(chan struct{}, 4)
	g.playerSubs.Store(id, ch)
	return ch
}
func (g *Game) UnsubscribePlayer(id string) { g.playerSubs.Delete(id) }
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

// HasActiveRound returns true if a round is currently open.
func (g *Game) HasActiveRound() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Round != nil && !g.Round.Ended
}

// HasPreviousRound returns true if at least one round has been played.
func (g *Game) HasPreviousRound() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Number > 0
}

// CurrentTrackURI returns the URI of the currently-playing round's track, or "".
func (g *Game) CurrentTrackURI() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Round == nil {
		return ""
	}
	return g.Round.Track.URI
}

// StartRound opens a new round for the given track. Cancels any pending
// auto-advance so a manual start beats the timer cleanly.
func (g *Game) StartRound(track SpotifyTrack) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.autoAdvanceTimer != nil {
		g.autoAdvanceTimer.Stop()
		g.autoAdvanceTimer = nil
	}
	g.Number++
	g.Round = &Round{
		Number:    g.Number,
		Track:     track,
		Answers:   make(map[string]*Answer),
		StartedAt: time.Now(),
	}
	go g.notify()
}

// CancelAutoAdvance stops any pending auto-advance timer. Used when the admin
// manually starts the next round during the results period.
func (g *Game) CancelAutoAdvance() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.autoAdvanceTimer != nil {
		g.autoAdvanceTimer.Stop()
		g.autoAdvanceTimer = nil
	}
}

// SubmitAnswer records a player's guess. Returns roundClosed=true if this
// submission ended the round outright (everyone has now answered).
func (g *Game) SubmitAnswer(playerID, songGuess, artistGuess string) bool {
	g.mu.Lock()
	if g.Round == nil || g.Round.Ended {
		g.mu.Unlock()
		return false
	}
	p, ok := g.Players[playerID]
	if !ok {
		g.mu.Unlock()
		return false
	}
	if _, already := g.Round.Answers[playerID]; already {
		g.mu.Unlock()
		return false
	}
	songOK := fuzzyMatch(songGuess, g.Round.Track.Name)
	artistOK := matchAnyArtist(artistGuess, g.Round.Track.ArtistNames())
	g.Round.Answers[playerID] = &Answer{
		PlayerID:      playerID,
		PlayerName:    p.Name,
		SongGuess:     songGuess,
		ArtistGuess:   artistGuess,
		SongCorrect:   songOK,
		ArtistCorrect: artistOK,
		SubmittedAt:   time.Now(),
	}
	answered := len(g.Round.Answers)
	total := len(g.Players)
	closed := false
	switch {
	case total > 0 && answered >= total:
		// everyone has answered — close immediately
		g.endRoundLocked()
		closed = true
	case total > 0 && answered*2 >= total && g.Round.GraceUntil.IsZero():
		// first time we've hit 50% — start grace period
		g.Round.GraceUntil = time.Now().Add(graceDuration)
		g.graceTimer = time.AfterFunc(graceDuration, g.endRoundByTimer)
	}
	g.mu.Unlock()
	go g.notify()
	return closed
}

// EndRound closes the active round immediately (used by admin "end now").
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

func (g *Game) endRoundByTimer() {
	g.mu.Lock()
	if g.Round == nil || g.Round.Ended {
		g.mu.Unlock()
		return
	}
	g.endRoundLocked()
	g.mu.Unlock()
	go g.notify()
}

// must be called with g.mu held
func (g *Game) endRoundLocked() {
	if g.graceTimer != nil {
		g.graceTimer.Stop()
		g.graceTimer = nil
	}
	g.Round.Ended = true
	g.Round.EndedAt = time.Now()
	g.Round.AutoAdvanceAt = time.Now().Add(resultsDuration)
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
	if g.onAutoAdvance != nil {
		g.autoAdvanceTimer = time.AfterFunc(resultsDuration, g.onAutoAdvance)
	}
	if g.onRoundEnd != nil {
		go g.onRoundEnd()
	}
}

// --- state snapshots for rendering/SSE ---

type PlayerView struct {
	RoundNumber      int       `json:"round_number"`
	RoundActive      bool      `json:"round_active"`
	RoundEnded       bool      `json:"round_ended"`
	HasAnswered      bool      `json:"has_answered"`
	YourSongOK       bool      `json:"your_song_ok"`
	YourArtistOK     bool      `json:"your_artist_ok"`
	YourSongGuess    string    `json:"your_song_guess"`
	YourArtistGuess  string    `json:"your_artist_guess"`
	AnswerCount      int       `json:"answer_count"`
	PlayerCount      int       `json:"player_count"`
	GraceUntilUnix   int64     `json:"grace_until,omitempty"`
	AutoAdvanceUnix  int64     `json:"auto_advance_at,omitempty"`
	RevealSong       string    `json:"reveal_song,omitempty"`
	RevealArtists    string    `json:"reveal_artists,omitempty"`
	Scoreboard       []*Player `json:"scoreboard"`
	You              *Player   `json:"you"`
}

func (g *Game) PlayerView(playerID string) PlayerView {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := PlayerView{PlayerCount: len(g.Players)}
	if p, ok := g.Players[playerID]; ok {
		v.You = p
	}
	if g.Round != nil {
		v.RoundNumber = g.Round.Number
		v.RoundActive = !g.Round.Ended
		v.RoundEnded = g.Round.Ended
		v.AnswerCount = len(g.Round.Answers)
		if !g.Round.GraceUntil.IsZero() && !g.Round.Ended {
			v.GraceUntilUnix = g.Round.GraceUntil.Unix()
		}
		if !g.Round.AutoAdvanceAt.IsZero() && g.Round.Ended {
			v.AutoAdvanceUnix = g.Round.AutoAdvanceAt.Unix()
		}
		if ans, ok := g.Round.Answers[playerID]; ok {
			v.HasAnswered = true
			v.YourSongGuess = ans.SongGuess
			v.YourArtistGuess = ans.ArtistGuess
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
	Authorized      bool      `json:"authorized"`
	RoundNumber     int       `json:"round_number"`
	RoundActive     bool      `json:"round_active"`
	RoundEnded      bool      `json:"round_ended"`
	CurrentSong     string    `json:"current_song"`
	CurrentArtists  string    `json:"current_artists"`
	AnswerCount     int       `json:"answer_count"`
	PlayerCount     int       `json:"player_count"`
	GraceUntilUnix  int64     `json:"grace_until,omitempty"`
	AutoAdvanceUnix int64     `json:"auto_advance_at,omitempty"`
	Answers         []*Answer `json:"answers"`
	Scoreboard      []*Player `json:"scoreboard"`
}

func (g *Game) AdminView() AdminView {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := AdminView{PlayerCount: len(g.Players)}
	if g.Round != nil {
		v.RoundNumber = g.Round.Number
		v.RoundActive = !g.Round.Ended
		v.RoundEnded = g.Round.Ended
		v.CurrentSong = g.Round.Track.Name
		v.CurrentArtists = joinArtists(g.Round.Track.ArtistNames())
		v.AnswerCount = len(g.Round.Answers)
		if !g.Round.GraceUntil.IsZero() && !g.Round.Ended {
			v.GraceUntilUnix = g.Round.GraceUntil.Unix()
		}
		if !g.Round.AutoAdvanceAt.IsZero() && g.Round.Ended {
			v.AutoAdvanceUnix = g.Round.AutoAdvanceAt.Unix()
		}
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
