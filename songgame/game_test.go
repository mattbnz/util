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
