# songgame

A local web server that runs a family "guess that song" game, driven by your
own Spotify Premium account.

## How it works

- **You** run `songgame` on your laptop. Spotify runs on whatever device you
  want the music to come out of (your laptop, a Spotify Connect speaker, etc.).
- **Players** (family on the couch, or on their phones on the same Wi‑Fi) open
  the root URL (e.g. `http://<your-laptop-ip>:8080/`) and enter a name.
- **You** (the host) open `/admin`, log in with Spotify, pick a playlist and a
  playback device, and click "Start next round".
- Each round the server picks a random unplayed track from the playlist and
  tells Spotify to play it. Players type the song title and artist. As soon as
  half the players have answered (or you hit "End round now"), the round closes
  and Spotify is paused. Correct song = 1 point, correct artist = 1 point.
- The player page **never** shows the currently-playing song or artist — only
  the admin page does. Keep your Spotify client out of sight.

## Requirements

- Go 1.19+
- A Spotify **Premium** account (free accounts cannot control playback via the
  Web API)
- A Spotify developer app — create one at
  <https://developer.spotify.com/dashboard>

## Setup

1. In your Spotify dev app dashboard, add `http://127.0.0.1:8080/admin/callback`
   as a Redirect URI (or whatever host/port you plan to run on).
2. Add your family members' Spotify accounts as users of the app (Dev Mode
   allows up to 5). They don't actually log in — only the host does — but as
   of Feb 2026 apps are scoped that way.
3. Grab the Client ID and Client Secret from the dashboard.

## Run

```sh
cd songgame
export SPOTIFY_CLIENT_ID=...
export SPOTIFY_CLIENT_SECRET=...
go run .
```

Flags:

- `-addr` listen address, default `:8080`
- `-redirect-base` the base URL Spotify should redirect back to; must match
  what you registered. Default `http://127.0.0.1:8080`. If you want players on
  other devices to reach the server at `http://192.168.x.x:8080/`, set
  `-redirect-base http://192.168.x.x:8080` **and** register that exact URI in
  the Spotify dashboard.

Then:

- Open `http://127.0.0.1:8080/admin` and click "Log in with Spotify". The first
  browser to reach `/admin` gets the admin cookie for this server run; restart
  the server to rotate.
- Open Spotify on the device you want the music to play from — it must be
  running and signed in, otherwise it won't show up in the device list.
- Pick a playlist and a device in the admin UI.
- Share the player URL with everyone: `http://<your-laptop-ip>:8080/`.
- Click "Start next round" to play a random track and open guessing.

## Matching

Guesses are normalised before comparison: lowercased, parenthetical notes
stripped (`(feat. X)`, `(Remastered 2011)`), trailing `- Remaster` annotations
stripped, leading `the`/`a`/`an` stripped, punctuation removed. A small edit
distance is also allowed (so "bohemian rhapsod" still counts). For artists,
matching any one artist of a multi-artist track is enough.

## Limitations

- In-memory state only — restarting the server resets scores and player list.
- One game at a time (one playlist, one set of players). Fine for a family.
- The admin UI auto-refreshes on game events, which will reset a half-chosen
  dropdown; pick playlist/device once at the start.
- If the Spotify client shows the track name on screen, the answer is visible.
  Keep the client minimized or on another device.
