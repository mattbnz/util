package main

import (
	"sort"
	"sync"
	"time"
)

// Default timings for the round lifecycle. Grace runs after 50% of players
// answer; results period runs after the round ends before auto-advancing.
// Both are configurable per-game via SetDurations.
const (
	defaultGraceDuration   = 30 * time.Second
	defaultResultsDuration = 30 * time.Second
	minDuration            = 5 * time.Second
	maxDuration            = 5 * time.Minute
)

type Player struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Score    int       `json:"score"`
	JoinedAt time.Time `json:"joined_at"`
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

// RoundResult is the persisted snapshot of a completed round, shown below the
// scoreboard so players can see how everyone guessed.
type RoundResult struct {
	Number  int       `json:"number"`
	Song    string    `json:"song"`
	Artists string    `json:"artists"`
	Answers []*Answer `json:"answers"`
}

// GameRecord is an archived, completed game session: all the rounds that were
// played, plus each player's final score at the moment the game ended.
type GameRecord struct {
	ID        string         `json:"id"`
	StartedAt time.Time      `json:"started_at"`
	EndedAt   time.Time      `json:"ended_at"`
	Players   []Player       `json:"players"`
	Rounds    []*RoundResult `json:"rounds"`
}

// LiveAnswer is a narrow view of an in-progress answer, exposed to players
// who have already submitted. Correctness is deliberately omitted so watching
// the live feed doesn't leak the right answer.
type LiveAnswer struct {
	PlayerID    string `json:"player_id"`
	PlayerName  string `json:"player_name"`
	SongGuess   string `json:"song_guess"`
	ArtistGuess string `json:"artist_guess"`
}

type Game struct {
	mu sync.Mutex

	// Active game lifecycle — GameID/StartedAt are set on the first round of
	// a game session and cleared when the session is archived to History.
	GameID    string
	StartedAt time.Time

	Round     *Round
	Rounds    []*RoundResult // all completed rounds in the current game session
	PrevRound *RoundResult   // convenience pointer to the most recent completed round
	History   []*GameRecord  // archived, finished games

	Players map[string]*Player
	Number  int

	graceDuration   time.Duration
	resultsDuration time.Duration

	graceTimer       *time.Timer
	autoAdvanceTimer *time.Timer
	onRoundEnd       func()
	onAutoAdvance    func()
	onChange         func()

	playerSubs sync.Map
	adminSubs  sync.Map
}

func NewGame() *Game {
	return &Game{
		Players:         make(map[string]*Player),
		graceDuration:   defaultGraceDuration,
		resultsDuration: defaultResultsDuration,
	}
}

// SetChangeCallback registers a hook fired after any state change. Used by
// the persistent store to mark itself dirty.
func (g *Game) SetChangeCallback(fn func()) {
	g.mu.Lock()
	g.onChange = fn
	g.mu.Unlock()
}

// Durations returns the current grace and results durations.
func (g *Game) Durations() (time.Duration, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.graceDuration, g.resultsDuration
}

// SetDurations updates the grace and results durations, clamped to a sane range.
func (g *Game) SetDurations(grace, results time.Duration) {
	grace = clampDuration(grace)
	results = clampDuration(results)
	g.mu.Lock()
	g.graceDuration = grace
	g.resultsDuration = results
	g.mu.Unlock()
	go g.notify()
}

func clampDuration(d time.Duration) time.Duration {
	if d < minDuration {
		return minDuration
	}
	if d > maxDuration {
		return maxDuration
	}
	return d
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
	g.mu.Lock()
	fn := g.onChange
	g.mu.Unlock()
	if fn != nil {
		fn()
	}
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

// Snapshot captures persistable game state. The in-progress round is not
// persisted — on restart we resume with scores and players intact but no
// active round.
type StateSnapshot struct {
	Players          []Player     `json:"players"`
	RoundNumber      int          `json:"round_number"`
	GraceDurationS   int          `json:"grace_duration_s"`
	ResultsDurationS int          `json:"results_duration_s"`
	PrevRound        *RoundResult `json:"prev_round,omitempty"`

	// Game session lifecycle.
	GameID    string         `json:"game_id,omitempty"`
	StartedAt time.Time      `json:"started_at,omitempty"`
	Rounds    []*RoundResult `json:"rounds,omitempty"`
	History   []*GameRecord  `json:"history,omitempty"`

	// Server-level persistence: the admin token (so the shared URL survives
	// restarts) and the Spotify refresh token (so the host doesn't have to
	// re-authenticate). Populated via Server.Snapshot, ignored by Game.
	AdminToken          string `json:"admin_token,omitempty"`
	SpotifyRefreshToken string `json:"spotify_refresh_token,omitempty"`
}

func (g *Game) Snapshot() StateSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	players := make([]Player, 0, len(g.Players))
	for _, p := range g.Players {
		players = append(players, *p)
	}
	return StateSnapshot{
		Players:          players,
		RoundNumber:      g.Number,
		GraceDurationS:   int(g.graceDuration / time.Second),
		ResultsDurationS: int(g.resultsDuration / time.Second),
		PrevRound:        g.PrevRound,
		GameID:           g.GameID,
		StartedAt:        g.StartedAt,
		Rounds:           append([]*RoundResult{}, g.Rounds...),
		History:          append([]*GameRecord{}, g.History...),
	}
}

// RestoreState applies a previously-saved snapshot. Should only be called
// before the game starts serving traffic.
func (g *Game) RestoreState(s StateSnapshot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s.GraceDurationS > 0 {
		g.graceDuration = clampDuration(time.Duration(s.GraceDurationS) * time.Second)
	}
	if s.ResultsDurationS > 0 {
		g.resultsDuration = clampDuration(time.Duration(s.ResultsDurationS) * time.Second)
	}
	g.Number = s.RoundNumber
	for i := range s.Players {
		p := s.Players[i]
		if p.ID == "" {
			continue
		}
		pp := p
		g.Players[p.ID] = &pp
	}
	g.GameID = s.GameID
	g.StartedAt = s.StartedAt
	if len(s.Rounds) > 0 {
		g.Rounds = append(g.Rounds, s.Rounds...)
	}
	if len(s.History) > 0 {
		g.History = append(g.History, s.History...)
	}
	// PrevRound defaults to the last completed round if the snapshot didn't
	// carry it explicitly.
	if s.PrevRound != nil {
		g.PrevRound = s.PrevRound
	} else if len(g.Rounds) > 0 {
		g.PrevRound = g.Rounds[len(g.Rounds)-1]
	}
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

// UpdateRoundTrack swaps the active round's track and re-grades any answers
// already submitted. Used by the admin resync action when Spotify is playing
// a different track than the round was opened with.
func (g *Game) UpdateRoundTrack(track SpotifyTrack) (updated bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Round == nil || g.Round.Ended {
		return false
	}
	if g.Round.Track.URI == track.URI {
		return false
	}
	g.Round.Track = track
	for _, ans := range g.Round.Answers {
		ans.SongCorrect = fuzzyMatch(ans.SongGuess, track.Name)
		ans.ArtistCorrect = matchAnyArtist(ans.ArtistGuess, track.ArtistNames())
	}
	go g.notify()
	return true
}

// StartRound opens a new round for the given track. Cancels any pending
// auto-advance so a manual start beats the timer cleanly. Starts a fresh
// game session (GameID/StartedAt) if none is active.
func (g *Game) StartRound(track SpotifyTrack) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.autoAdvanceTimer != nil {
		g.autoAdvanceTimer.Stop()
		g.autoAdvanceTimer = nil
	}
	if g.GameID == "" {
		g.GameID = randomGameID()
		g.StartedAt = time.Now()
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

// EndGame archives the current game session to History and resets live state
// (scores, rounds, active round). Players are preserved across the transition
// so they don't have to re-join, but all scores return to zero. Safe to call
// when no game is active — it's a no-op then.
func (g *Game) EndGame() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.GameID == "" && len(g.Rounds) == 0 && (g.Round == nil || g.Round.Ended) {
		return
	}
	// End any live round first so it counts toward the archive.
	if g.Round != nil && !g.Round.Ended {
		g.endRoundLocked()
	}
	if g.autoAdvanceTimer != nil {
		g.autoAdvanceTimer.Stop()
		g.autoAdvanceTimer = nil
	}
	// Snapshot players with their final scores into the archive.
	players := make([]Player, 0, len(g.Players))
	for _, p := range g.Players {
		players = append(players, *p)
	}
	g.History = append(g.History, &GameRecord{
		ID:        g.GameID,
		StartedAt: g.StartedAt,
		EndedAt:   time.Now(),
		Players:   players,
		Rounds:    append([]*RoundResult{}, g.Rounds...),
	})
	// Reset the live state.
	g.GameID = ""
	g.StartedAt = time.Time{}
	g.Rounds = nil
	g.PrevRound = nil
	g.Round = nil
	g.Number = 0
	for _, p := range g.Players {
		p.Score = 0
	}
	go g.notify()
}

// EjectPlayer removes a player from the current game. If they'd submitted
// an answer in the active round it's discarded so the counts stay consistent.
// Their presence in any already-archived GameRecord is untouched.
func (g *Game) EjectPlayer(playerID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.Players[playerID]; !ok {
		return false
	}
	delete(g.Players, playerID)
	if g.Round != nil {
		delete(g.Round.Answers, playerID)
	}
	go g.notify()
	return true
}

// randomGameID produces a short human-friendly game id (8 hex chars).
func randomGameID() string { return randomHex(4) }

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
		g.Round.GraceUntil = time.Now().Add(g.graceDuration)
		g.graceTimer = time.AfterFunc(g.graceDuration, g.endRoundByTimer)
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
	g.Round.AutoAdvanceAt = time.Now().Add(g.resultsDuration)
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
	// Capture a persistent snapshot of this round's answers so players can see
	// everyone's guesses below the scoreboard — including after the next round starts.
	result := &RoundResult{
		Number:  g.Round.Number,
		Song:    g.Round.Track.Name,
		Artists: joinArtists(g.Round.Track.ArtistNames()),
	}
	for _, a := range g.Round.Answers {
		ac := *a
		result.Answers = append(result.Answers, &ac)
	}
	sort.Slice(result.Answers, func(i, j int) bool {
		return result.Answers[i].SubmittedAt.Before(result.Answers[j].SubmittedAt)
	})
	g.PrevRound = result
	g.Rounds = append(g.Rounds, result)
	if g.onAutoAdvance != nil {
		g.autoAdvanceTimer = time.AfterFunc(g.resultsDuration, g.onAutoAdvance)
	}
	if g.onRoundEnd != nil {
		go g.onRoundEnd()
	}
}

// --- state snapshots for rendering/SSE ---

type PlayerView struct {
	RoundNumber     int           `json:"round_number"`
	RoundActive     bool          `json:"round_active"`
	RoundEnded      bool          `json:"round_ended"`
	HasAnswered     bool          `json:"has_answered"`
	YourSongOK      bool          `json:"your_song_ok"`
	YourArtistOK   bool          `json:"your_artist_ok"`
	YourSongGuess   string        `json:"your_song_guess"`
	YourArtistGuess string        `json:"your_artist_guess"`
	AnswerCount     int           `json:"answer_count"`
	PlayerCount     int           `json:"player_count"`
	GraceUntilUnix  int64         `json:"grace_until,omitempty"`
	AutoAdvanceUnix int64         `json:"auto_advance_at,omitempty"`
	RevealSong      string        `json:"reveal_song,omitempty"`
	RevealArtists   string        `json:"reveal_artists,omitempty"`
	Scoreboard      []*Player     `json:"scoreboard"`
	You             *Player       `json:"you"`
	PrevRound       *RoundResult  `json:"prev_round,omitempty"`
	LiveAnswers     []*LiveAnswer `json:"live_answers,omitempty"`
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
			// Expose other players' guesses (no correctness) to this player
			// while the round is still live — gives them something to watch
			// and reveals nothing about the right answer.
			if !g.Round.Ended {
				for _, a := range g.Round.Answers {
					v.LiveAnswers = append(v.LiveAnswers, &LiveAnswer{
						PlayerID:    a.PlayerID,
						PlayerName:  a.PlayerName,
						SongGuess:   a.SongGuess,
						ArtistGuess: a.ArtistGuess,
					})
				}
				sort.Slice(v.LiveAnswers, func(i, j int) bool {
					return g.Round.Answers[v.LiveAnswers[i].PlayerID].SubmittedAt.Before(
						g.Round.Answers[v.LiveAnswers[j].PlayerID].SubmittedAt)
				})
			}
		}
		if g.Round.Ended {
			v.RevealSong = g.Round.Track.Name
			v.RevealArtists = joinArtists(g.Round.Track.ArtistNames())
		}
	}
	v.Scoreboard = g.playerListLocked()
	v.PrevRound = g.PrevRound
	return v
}

type AdminView struct {
	Authorized      bool          `json:"authorized"`
	GameID          string        `json:"game_id,omitempty"`
	GameActive      bool          `json:"game_active"`
	RoundNumber     int           `json:"round_number"`
	RoundActive     bool          `json:"round_active"`
	RoundEnded      bool          `json:"round_ended"`
	CurrentSong     string        `json:"current_song"`
	CurrentArtists  string        `json:"current_artists"`
	AnswerCount     int           `json:"answer_count"`
	PlayerCount     int           `json:"player_count"`
	GraceUntilUnix  int64         `json:"grace_until,omitempty"`
	AutoAdvanceUnix int64         `json:"auto_advance_at,omitempty"`
	Answers         []*Answer     `json:"answers"`
	Scoreboard      []*Player     `json:"scoreboard"`
	PrevRound       *RoundResult  `json:"prev_round,omitempty"`
	History         []*GameRecord `json:"history,omitempty"`
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
	v.PrevRound = g.PrevRound
	v.GameID = g.GameID
	v.GameActive = g.GameID != ""
	// History most-recent-first, a copy so the caller can't mutate.
	if len(g.History) > 0 {
		rev := make([]*GameRecord, 0, len(g.History))
		for i := len(g.History) - 1; i >= 0; i-- {
			rev = append(rev, g.History[i])
		}
		v.History = rev
	}
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
