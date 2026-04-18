# songgame

A local web server that runs a family "guess that song" game. You drive the
music from Spotify however you like — phone, laptop, Connect speaker — and the
server just reads what's playing and does play / pause / skip between rounds.

## How it works

- **You** run `songgame` on your laptop.
- **You** open any playlist on Spotify (phone is easiest), turn on shuffle,
  and hit play. The app takes it from there.
- **Players** open the root URL (e.g. `http://<your-laptop-ip>:8080/`) on their
  phones, enter a name, and wait.
- **You** open `/admin`, log in with Spotify once, and click "Start next
  round". The server calls *skip → resume → read currently-playing*, records
  the track as the round's answer, and opens guessing for the players.
- Players submit song + artist guesses. Once half have answered, a **30-second
  grace period** starts so the slower half can still guess. The round ends
  when the grace period expires, everyone has answered, or you click "End
  round now".
- When the round ends, the server **ducks Spotify's volume** to a background
  level (the song keeps playing) and reveals the answer. **30 seconds later
  the next round auto-starts** — volume is restored, Spotify skips to a new
  track, and a fresh round opens. You can also click "Start next round" to
  skip the results countdown.
- Scoring: 1 point for the correct song, 1 for the artist.
- The player page **never** shows the currently-playing song or artist. The
  answer is only visible on `/admin`. Keep Spotify out of sight of players.

## Requirements

- Go 1.19+
- A Spotify **Premium** account (free accounts can't control playback via the
  Web API)
- A Spotify developer app — create one at
  <https://developer.spotify.com/dashboard>. Add
  `http://127.0.0.1:8080/admin/callback` as a Redirect URI (or whatever host
  and port you plan to use).

## Run

```sh
cd songgame
export SPOTIFY_CLIENT_ID=...
export SPOTIFY_CLIENT_SECRET=...
go run .
```

Flags:

- `-addr` listen address, default `:8080`
- `-redirect-base` OAuth redirect base URL; must match what you registered.
  Default `http://127.0.0.1:8080`. If you want players on other devices to
  reach the server at e.g. `http://192.168.x.x:8080/`, run with
  `-redirect-base http://192.168.x.x:8080` **and** add that exact URI to your
  Spotify app's Redirect URIs.
- `-state` path to the JSON state file, default `songgame-state.json`. Players,
  scores, round number, configurable durations, the admin token, and the
  Spotify refresh token are saved on every change (debounced by 2 seconds)
  and flushed on SIGINT/SIGTERM. Pass an empty string to disable persistence.
  The file is written with mode 0600 because the Spotify refresh token
  functions as a credential; keep it on a disk you control.

## Settings

The admin page has a "Settings" card with two fields (5–300 seconds each):

- **Grace period after 50%** — how long the slower half of players have to
  guess once the quicker half has locked in. Default 30s.
- **Results / auto-advance** — how long results stay on screen before the next
  round auto-starts. Default 30s.

Both are persisted in the state file, so they survive a restart.

Then:

1. On startup the server logs a shareable admin URL, e.g.
   `http://127.0.0.1:8080/admin?t=<token>`. Open it to claim admin access
   (the token is swapped for a cookie and stripped from the URL). Forward
   the same link to anyone else who should co-host — the admin page also
   has a "Copy" button for it. The token rotates on every server restart.
2. Click "Log in with Spotify" on the admin page.
3. On Spotify (your phone works well), pick a playlist, enable shuffle, tap
   play. The device must be active for the API to find it.
4. Players join at `http://<your-laptop-ip>:8080/`.
5. Click "Start round 1". On subsequent rounds the server will skip to a new
   song automatically.

## Matching

Guesses are normalised before comparison: lowercased, parenthetical notes
stripped (`(feat. X)`, `(Remastered 2011)`), trailing `- Remaster` annotations
stripped, leading `the`/`a`/`an` stripped, punctuation removed. A small edit
distance is allowed (so "bohemian rhapsod" still counts). For artists,
matching any one artist of a multi-artist track is enough.

## Debugging Spotify sync issues

If the round's expected track drifts from what's actually playing (e.g. you
transferred Spotify Connect to a different device, or the API lagged when the
round was opened), the admin page has tools to recover:

- The **Spotify playback state** card polls `/me/player` every few seconds
  and shows the active device, volume, play/pause state, shuffle flag, and
  the track Spotify *thinks* is playing with progress. If that track doesn't
  match the round's expected track you'll see a red mismatch warning.
- The server **auto-resyncs** in the background: every 5 seconds it polls
  `/me/player` and, if the reported track doesn't match the round's expected
  track across two consecutive polls (≈10s of drift), it swaps the round's
  answer to the actually-playing track and re-grades every submitted guess.
- The **Resync with Spotify** button forces the same reconciliation on
  demand — use it if you don't want to wait for auto-resync to kick in.
- Every Spotify API call is logged with method, path, status code, and
  duration; error bodies are included on 4xx/5xx. Run the server in a
  terminal and tail the output while you debug.

## Limitations

- One game at a time.
- Spotify's `currently-playing` endpoint can be a second or two stale after a
  skip; the server polls for up to ~4s to catch the new track.
- If Spotify isn't actively playing when you click "Start next round", the
  skip/play call fails — the admin page will show the error, and you just
  need to nudge play on your phone and try again.
- Volume ducking relies on Spotify Connect volume control, which doesn't work
  on every device (Bluetooth speakers in particular may ignore it). If it
  silently fails, the song just keeps playing at normal volume during results.
