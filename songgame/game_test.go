package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func track(id, name string, artists ...string) SpotifyTrack {
	tr := SpotifyTrack{ID: id, URI: "spotify:track:" + id, Name: name}
	for _, a := range artists {
		tr.Artists = append(tr.Artists, struct {
			Name string `json:"name"`
		}{Name: a})
	}
	return tr
}

func TestHalfAnsweredStartsGracePeriodNotImmediateEnd(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")
	g.AddOrUpdatePlayer("c", "Carol")
	g.AddOrUpdatePlayer("d", "Dave")
	g.StartRound(track("t1", "Imagine", "John Lennon"))

	// Two of four answer → 50% hit → grace period should start, round not ended
	closed1 := g.SubmitAnswer("a", "Imagine", "John Lennon")
	closed2 := g.SubmitAnswer("b", "Imagine", "Lennon")
	if closed1 || closed2 {
		t.Fatalf("SubmitAnswer unexpectedly closed the round: %v %v", closed1, closed2)
	}
	v := g.AdminView()
	if !v.RoundActive || v.RoundEnded {
		t.Fatalf("round should still be active during grace; got active=%v ended=%v", v.RoundActive, v.RoundEnded)
	}
	if v.GraceUntilUnix == 0 {
		t.Fatalf("grace_until should be set once 50%% have answered")
	}
}

func TestEveryoneAnsweredClosesRoundImmediately(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")
	g.StartRound(track("t1", "Imagine", "John Lennon"))

	g.SubmitAnswer("a", "Imagine", "John Lennon")
	closed := g.SubmitAnswer("b", "Imagine", "Lennon")
	if !closed {
		t.Fatalf("SubmitAnswer should have closed the round once all players answered")
	}
	v := g.AdminView()
	if v.RoundActive || !v.RoundEnded {
		t.Fatalf("round should be ended; got active=%v ended=%v", v.RoundActive, v.RoundEnded)
	}
}

func TestRoundEndTriggersCallbacksAndSchedulesAutoAdvance(t *testing.T) {
	var roundEnds, autoAdvances int32
	g := NewGame()
	g.SetCallbacks(
		func() { atomic.AddInt32(&roundEnds, 1) },
		func() { atomic.AddInt32(&autoAdvances, 1) },
	)
	g.AddOrUpdatePlayer("a", "A")
	g.StartRound(track("t1", "Song", "Artist"))

	// Only player answers → round closes immediately
	g.SubmitAnswer("a", "Song", "Artist")

	// onRoundEnd is called in a goroutine; give it a moment
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&roundEnds); got != 1 {
		t.Fatalf("onRoundEnd called %d times, want 1", got)
	}

	// Auto-advance shouldn't have fired yet (30s delay)
	if got := atomic.LoadInt32(&autoAdvances); got != 0 {
		t.Fatalf("onAutoAdvance fired too early: %d", got)
	}

	v := g.AdminView()
	if v.AutoAdvanceUnix == 0 {
		t.Fatalf("auto_advance_at should be set after round ends")
	}
}

func TestStartRoundCancelsAutoAdvanceTimer(t *testing.T) {
	var autoAdvances int32
	g := NewGame()
	g.SetCallbacks(nil, func() { atomic.AddInt32(&autoAdvances, 1) })
	g.AddOrUpdatePlayer("a", "A")

	g.StartRound(track("t1", "Song", "Artist"))
	g.SubmitAnswer("a", "Song", "Artist")
	// Round ended → auto-advance timer scheduled for 30s

	// Start next round manually — should cancel the timer
	g.StartRound(track("t2", "Song2", "Artist2"))
	v := g.AdminView()
	if v.AutoAdvanceUnix != 0 {
		t.Fatalf("AutoAdvanceUnix should be cleared for fresh round, got %d", v.AutoAdvanceUnix)
	}
}

func TestLiveAnswersOnlyVisibleToAnsweredPlayers(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")
	g.AddOrUpdatePlayer("c", "Carol")
	g.StartRound(track("t1", "Imagine", "John Lennon"))
	g.SubmitAnswer("a", "Imagine", "Lennon") // alice has answered

	// Alice has answered — she sees the live list.
	va := g.PlayerView("a")
	if len(va.LiveAnswers) != 1 {
		t.Fatalf("Alice should see 1 live answer; got %d", len(va.LiveAnswers))
	}
	if va.LiveAnswers[0].PlayerID != "a" || va.LiveAnswers[0].SongGuess != "Imagine" {
		t.Errorf("unexpected live answer: %+v", va.LiveAnswers[0])
	}

	// Bob hasn't answered — he must NOT see the live feed (would spoil his guess).
	vb := g.PlayerView("b")
	if len(vb.LiveAnswers) != 0 {
		t.Fatalf("Bob hasn't answered — LiveAnswers must be empty; got %+v", vb.LiveAnswers)
	}

	// LiveAnswer struct must not expose correctness fields.
	// Verified by type; enforce via JSON marshalling: the marshalled bytes
	// must not contain "song_correct" / "artist_correct".
	b, _ := json.Marshal(va.LiveAnswers[0])
	if strings.Contains(string(b), "correct") {
		t.Errorf("LiveAnswer JSON leaked correctness: %s", b)
	}
}

func TestUpdateRoundTrackRegradesAnswers(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")
	g.AddOrUpdatePlayer("c", "Carol") // never answers — keeps the round from auto-closing
	g.StartRound(track("t1", "Imagine", "John Lennon"))

	// Alice guesses the actually-playing song; Bob guesses something that won't match either track.
	g.SubmitAnswer("a", "Yesterday", "The Beatles")
	g.SubmitAnswer("b", "completely wrong", "someone else")

	v := g.AdminView()
	for _, a := range v.Answers {
		if a.SongCorrect || a.ArtistCorrect {
			t.Fatalf("before resync, no answer should be graded correct: %+v", a)
		}
	}

	// Admin resyncs: the track Spotify is actually playing was Yesterday by The Beatles.
	updated := g.UpdateRoundTrack(track("t2", "Yesterday", "The Beatles"))
	if !updated {
		t.Fatalf("UpdateRoundTrack should report an update")
	}

	v = g.AdminView()
	var alice, bob *Answer
	for _, a := range v.Answers {
		if a.PlayerID == "a" {
			alice = a
		} else if a.PlayerID == "b" {
			bob = a
		}
	}
	if alice == nil || !alice.SongCorrect || !alice.ArtistCorrect {
		t.Errorf("Alice's correct guess should be marked correct after resync: %+v", alice)
	}
	if bob == nil || bob.SongCorrect || bob.ArtistCorrect {
		t.Errorf("Bob's wrong guess should still be wrong: %+v", bob)
	}
}

func TestUpdateRoundTrackRefusesWhenNoActiveRound(t *testing.T) {
	g := NewGame()
	if g.UpdateRoundTrack(track("x", "Song", "Artist")) {
		t.Fatalf("UpdateRoundTrack should return false when no round is active")
	}
}

func TestBeginPrepRoundThenActivate(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "A")
	g.BeginPrepRound("")
	if g.RoundPhase() != PhasePrep {
		t.Fatalf("phase after BeginPrepRound: got %q, want prep", g.RoundPhase())
	}
	// Submissions during prep should be rejected.
	if g.SubmitAnswer("a", "x", "y") {
		t.Errorf("SubmitAnswer should return false during prep phase")
	}
	// Activate.
	g.ActivateRound(track("t1", "Song", "Artist"))
	if g.RoundPhase() != PhaseActive {
		t.Fatalf("phase after ActivateRound: got %q, want active", g.RoundPhase())
	}
	// Now submissions work.
	g.SubmitAnswer("a", "Song", "Artist")
	v := g.AdminView()
	if len(v.Answers) != 1 {
		t.Errorf("expected 1 answer after activate+submit, got %d", len(v.Answers))
	}
}

func TestCancelPrepRoundRewindsNumber(t *testing.T) {
	g := NewGame()
	g.BeginPrepRound("")
	n := g.Number
	if !g.CancelPrepRound() {
		t.Fatalf("CancelPrepRound should succeed for a prep round")
	}
	if g.Number != n-1 {
		t.Errorf("Number should rewind after cancel: got %d, want %d", g.Number, n-1)
	}
	if g.Round != nil {
		t.Errorf("Round should be cleared after cancel")
	}
}

func TestEndGameArchivesAndResets(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")
	g.StartRound(track("t1", "Imagine", "John Lennon"))
	g.SubmitAnswer("a", "Imagine", "Lennon")
	g.SubmitAnswer("b", "Imagine", "Lennon")
	// Both answered → round closed. GameID is set by StartRound.
	if g.GameID == "" {
		t.Fatalf("GameID should be set after starting a round")
	}
	priorID := g.GameID

	g.EndGame()

	if g.GameID != "" {
		t.Errorf("GameID should be cleared after EndGame, got %q", g.GameID)
	}
	if len(g.History) != 1 {
		t.Fatalf("history should contain one record, got %d", len(g.History))
	}
	rec := g.History[0]
	if rec.ID != priorID {
		t.Errorf("archived game id: got %q, want %q", rec.ID, priorID)
	}
	if len(rec.Rounds) != 1 {
		t.Errorf("archived record rounds: got %d, want 1", len(rec.Rounds))
	}
	if len(rec.Players) != 2 {
		t.Errorf("archived record players: got %d, want 2", len(rec.Players))
	}
	for _, p := range g.Players {
		if p.Score != 0 {
			t.Errorf("player %s score should reset to 0, got %d", p.Name, p.Score)
		}
	}
	if len(g.Rounds) != 0 {
		t.Errorf("live rounds should be cleared, got %d", len(g.Rounds))
	}
	if g.Number != 0 {
		t.Errorf("round number should reset to 0, got %d", g.Number)
	}
}

func TestEjectPlayerRemovesFromPlayersAndActiveRound(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")
	g.AddOrUpdatePlayer("c", "Carol")
	g.StartRound(track("t1", "Imagine", "John Lennon"))
	g.SubmitAnswer("a", "Imagine", "Lennon")

	if !g.EjectPlayer("a") {
		t.Fatalf("EjectPlayer should return true for a known player")
	}
	if _, ok := g.Players["a"]; ok {
		t.Errorf("Alice should be gone from Players")
	}
	if g.Round == nil {
		t.Fatalf("round should still be active")
	}
	if _, ok := g.Round.Answers["a"]; ok {
		t.Errorf("Alice's answer should be removed from the active round")
	}
	if g.EjectPlayer("nonexistent") {
		t.Errorf("EjectPlayer should return false for unknown id")
	}
}

func TestStartRoundAssignsGameIDOnce(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "A")
	g.StartRound(track("t1", "One", "X"))
	first := g.GameID
	if first == "" {
		t.Fatalf("GameID should be set after first StartRound")
	}
	// End the round then start another; GameID must persist through the game.
	g.EndRound()
	g.StartRound(track("t2", "Two", "X"))
	if g.GameID != first {
		t.Errorf("GameID should remain %q across rounds of the same game, got %q", first, g.GameID)
	}
}

func TestPrevRoundCapturedOnEndAndPersistsIntoNextRound(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")

	g.StartRound(track("t1", "Imagine", "John Lennon"))
	g.SubmitAnswer("a", "Imagine", "John Lennon")
	g.SubmitAnswer("b", "wrong", "wrong")

	// Everyone answered → round ends immediately; prev round snapshot populated
	v := g.PlayerView("a")
	if v.PrevRound == nil {
		t.Fatalf("PrevRound should be populated after round ends")
	}
	if v.PrevRound.Number != 1 || v.PrevRound.Song != "Imagine" {
		t.Fatalf("unexpected prev round: %+v", v.PrevRound)
	}
	if n := len(v.PrevRound.Answers); n != 2 {
		t.Fatalf("expected 2 answers, got %d", n)
	}

	// Start the next round; prev round should still be visible.
	g.StartRound(track("t2", "Yesterday", "The Beatles"))
	v = g.PlayerView("a")
	if v.PrevRound == nil || v.PrevRound.Number != 1 {
		t.Fatalf("PrevRound should still reference round 1 during round 2, got %+v", v.PrevRound)
	}
}

func TestDraftIsPromotedWhenRoundEndsWithoutSubmit(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")
	g.StartRound(track("t1", "Imagine", "John Lennon"))

	// Bob has typed something but never hit Submit.
	if !g.UpdateDraft("b", "Imagin", "John Lennon") {
		t.Fatalf("UpdateDraft should succeed during active round")
	}
	// Alice submits; admin then ends the round before Bob can finish.
	g.SubmitAnswer("a", "Imagine", "Lennon")
	g.EndRound()

	v := g.AdminView()
	var bob *Answer
	for _, a := range v.Answers {
		if a.PlayerID == "b" {
			bob = a
		}
	}
	if bob == nil {
		t.Fatalf("Bob's draft should have been promoted to an answer")
	}
	if bob.SongGuess != "Imagin" || bob.ArtistGuess != "John Lennon" {
		t.Errorf("promoted draft text wrong: %+v", bob)
	}
	if !bob.SongCorrect || !bob.ArtistCorrect {
		t.Errorf("promoted draft should be graded; got song=%v artist=%v", bob.SongCorrect, bob.ArtistCorrect)
	}
	if g.Players["b"].Score != 2 {
		t.Errorf("Bob should have scored 2 from his promoted draft, got %d", g.Players["b"].Score)
	}
}

func TestExplicitAnswerWinsOverDraft(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob") // never answers — keeps round open
	g.StartRound(track("t1", "Imagine", "John Lennon"))

	g.UpdateDraft("a", "wrong", "wrong")
	g.SubmitAnswer("a", "Imagine", "Lennon")
	// Late draft after submission is ignored — already submitted.
	if g.UpdateDraft("a", "later draft", "later") {
		t.Errorf("UpdateDraft should be a no-op once the player has formally submitted")
	}
	g.EndRound()

	v := g.AdminView()
	var alice *Answer
	for _, a := range v.Answers {
		if a.PlayerID == "a" {
			alice = a
		}
	}
	if alice == nil || alice.SongGuess != "Imagine" {
		t.Fatalf("Alice's explicit submission should win, got %+v", alice)
	}
}

func TestEmptyDraftIsNotPromoted(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "Alice")
	g.AddOrUpdatePlayer("b", "Bob")
	g.StartRound(track("t1", "Imagine", "John Lennon"))

	// Bob's client posts a draft but they cleared the inputs.
	g.UpdateDraft("b", "", "")
	g.SubmitAnswer("a", "Imagine", "Lennon")
	g.EndRound()

	v := g.AdminView()
	for _, a := range v.Answers {
		if a.PlayerID == "b" {
			t.Fatalf("empty draft should not promote to an answer: %+v", a)
		}
	}
}

func TestDraftRefusedOutsideActiveRound(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "A")
	if g.UpdateDraft("a", "x", "y") {
		t.Errorf("UpdateDraft should be false with no round")
	}
	g.BeginPrepRound("")
	if g.UpdateDraft("a", "x", "y") {
		t.Errorf("UpdateDraft should be false during prep")
	}
	g.ActivateRound(track("t1", "Song", "Artist"))
	if !g.UpdateDraft("a", "x", "y") {
		t.Errorf("UpdateDraft should succeed during active")
	}
	g.EndRound()
	if g.UpdateDraft("a", "x", "y") {
		t.Errorf("UpdateDraft should be false after round ended")
	}
}

func TestGraceUntilClearsOnRoundEnd(t *testing.T) {
	g := NewGame()
	g.AddOrUpdatePlayer("a", "A")
	g.AddOrUpdatePlayer("b", "B")
	g.StartRound(track("t1", "Song", "Artist"))
	g.SubmitAnswer("a", "wrong", "wrong") // triggers 50% grace
	if g.AdminView().GraceUntilUnix == 0 {
		t.Fatalf("grace should be set")
	}
	g.EndRound()
	if v := g.AdminView(); v.GraceUntilUnix != 0 {
		t.Fatalf("grace_until should be cleared after round ends, got %d", v.GraceUntilUnix)
	}
}
