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
- Players submit song + artist guesses. The round ends once half the players
  have answered (or you click "End round now"). The server pauses playback and
  reveals the answer. Scoring: 1 point for the correct song, 1 for the artist.
- Click "Start next round" again — skip, resume, repeat.
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

Then:

1. Open `http://127.0.0.1:8080/admin` and click "Log in with Spotify". First
   browser to reach `/admin` owns the admin session; restart to rotate.
2. On Spotify (your phone works well), pick a playlist, enable shuffle, tap
   play. The device must be active for the API to find it.
3. Players join at `http://<your-laptop-ip>:8080/`.
4. Click "Start round 1". On subsequent rounds the server will skip to a new
   song automatically.

## Matching

Guesses are normalised before comparison: lowercased, parenthetical notes
stripped (`(feat. X)`, `(Remastered 2011)`), trailing `- Remaster` annotations
stripped, leading `the`/`a`/`an` stripped, punctuation removed. A small edit
distance is allowed (so "bohemian rhapsod" still counts). For artists,
matching any one artist of a multi-artist track is enough.

## Limitations

- In-memory state only — restarting the server resets scores and players.
- One game at a time.
- Spotify's `currently-playing` endpoint can be a second or two stale after a
  skip; the server polls for up to ~4s to catch the new track.
- If Spotify isn't actively playing when you click "Start next round", the
  skip/play call fails — the admin page will show the error, and you just
  need to nudge play on your phone and try again.
