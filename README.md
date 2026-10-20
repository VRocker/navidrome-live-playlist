# LivePlaylist

> **AI disclosure:** This project was created with assistance from AI.

LivePlaylist is a Navidrome WebAssembly plugin that takes a list of gigs, looks
up each band's setlist on setlist.fm, matches the songs against your Navidrome
library, and adds the matching tracks to one playlist. The playlist is extended
without removing tracks already in it, and duplicate tracks are not added twice.

## Requirements

- A Navidrome version with plugin support
- A setlist.fm API key
- TinyGo and Go versions compatible with the Navidrome plugin PDK

## Configure

1. Build and install `liveplaylist.ndp` as described below.
2. In Navidrome's Plugins administration page, enable LivePlaylist.
3. Add a user configuration for each person. Each configuration has that
   Navidrome username, their setlist.fm API key, playlist name (defaults to
   `Live Setlists`), a gig source (`manual` or `profile`), and a live recording
   preference and minimum track length.
4. Grant the plugin access to every configured user in the plugin's user access
   settings. The plugin also needs access to the libraries containing their music.
5. Set the global **Refresh interval** (defaults to weekly), then save. The
   plugin syncs once at startup and then at 03:00 according to the Navidrome
   server's clock: daily, every Sunday, or on the first day of each month.
   Changes to the selected source are picked up during the next sync. If you
   change the refresh interval, reload the plugin (or restart Navidrome) for
   the new schedule to take effect.

The plugin settings page is administered globally in Navidrome, but its
`users` list keeps each person's key, gigs, and playlist separate. When
upgrading from the earlier single-user configuration, move its username, API
key, playlist name, and gigs into one object in the new `users` list, and set
`importMode` to `manual`.

Choose `profile` to import all concerts that profile has marked as attended.
Enter the profile user ID from its URL, such as `some-user` in
`https://www.setlist.fm/user/some-user`—not the full URL. The plugin reads the
profile's attended setlists, including each band's name, date, and songs.
The API may wrap a setlist's entries in `sets.set`; the plugin handles that
shape as well as the direct `set` array. Requests are paced at least two
seconds apart to reduce the chance of hitting setlist.fm rate limits.

Choose `manual` to enter gigs yourself, each with an ISO date (`YYYY-MM-DD`)
and band name. The profile ID is ignored in manual mode, and manual gigs are
ignored in profile mode; select one source for each user.

For example, the plugin's configuration is structured like this:

```json
{
  "users": [
    {
      "username": "alice",
      "apiKey": "ALICE_SETLISTFM_API_KEY",
      "playlistName": "Alice - Live Setlists",
      "importMode": "profile",
      "liveVersionPreference": "preferLive",
      "minimumTrackLengthSeconds": 60,
      "setlistUserId": "alice-setlistfm-id"
    },
    {
      "username": "bob",
      "apiKey": "BOB_SETLISTFM_API_KEY",
      "importMode": "manual",
      "gigs": [
        { "date": "2025-07-20", "band": "Another Band" }
      ]
    }
  ]
}
```

Each user can use a separate setlist.fm key, source, playlist name, and live
recording preference and minimum track length. Set
`liveVersionPreference` to `any` (the default),
`preferLive`, or `avoidLive`. When the default Navidrome match does not meet
the preference, the plugin searches that user's library for another matching
track. It only switches when it finds the same title and artist and can
identify the requested version from the track title or library metadata
(including album or path); otherwise it keeps Navidrome's default match.
`minimumTrackLengthSeconds` defaults to `60`; newly matched tracks shorter
than that are not added. Set it to `0` to include tracks of any length. Neither
option removes tracks already in the playlist.
At least one manual gig is required in manual mode; a profile user ID is
required in profile mode. Profile setlists are paginated and refreshed on each
sync. If a setlist is not yet available on the profile, it will be imported on
the next sync after it is marked attended. Setlist songs missing from a user's
library are logged and skipped.

## Troubleshooting

The plugin logs each setlist.fm request and the number of setlists returned.
For manual gigs, it also logs the artist names and event dates returned by the
API when none pass the exact band/date match. HTTP errors include a short
response body (for example, to help identify an invalid API key). Check
Navidrome's plugin logs for these messages before changing the gig entries.
If the API returns HTTP 429, the log includes its `Retry-After` value when
provided; the failed sync can be retried at the next scheduled run.

## Build on Windows

From this directory, run:

```powershell
go mod tidy
tinygo build -o plugin.wasm -target wasip1 -buildmode=c-shared .
Compress-Archive -Path manifest.json,plugin.wasm -DestinationPath liveplaylist.ndp -Force
```

Place `liveplaylist.ndp` in Navidrome's plugins folder, then rescan for plugins.

## Build on Linux

Install Go, TinyGo, and `zip` versions compatible with the Navidrome plugin PDK.
From this directory, run:

```sh
go mod tidy
tinygo build -o plugin.wasm -target wasip1 -buildmode=c-shared .
zip -j -FS liveplaylist.ndp manifest.json plugin.wasm
```

Place `liveplaylist.ndp` in Navidrome's plugins folder, then rescan for plugins.

## Playlist behavior

The plugin updates one playlist per configured user. It preserves each
playlist's existing track order, appends newly matched tracks, and deduplicates
by Navidrome track ID. It does not remove tracks when gigs are removed from the
configuration.
