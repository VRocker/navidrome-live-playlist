package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/navidrome/navidrome/plugins/pdk/go/types"
)

func TestParseGigDate(t *testing.T) {
	date, err := parseGigDate("2025-06-14")
	if err != nil {
		t.Fatalf("parseGigDate returned an error: %v", err)
	}
	if date.Format("2006-01-02") != "2025-06-14" {
		t.Fatalf("unexpected parsed date: %s", date)
	}

	for _, invalid := range []string{"14-06-2025", "2025-02-30", "2025-6-14", ""} {
		if _, err := parseGigDate(invalid); err == nil {
			t.Errorf("parseGigDate(%q) should fail", invalid)
		}
	}
}

func TestRefreshCron(t *testing.T) {
	tests := []struct {
		interval string
		want     string
		wantErr  bool
	}{
		{interval: "", want: "0 3 * * 0"},
		{interval: "weekly", want: "0 3 * * 0"},
		{interval: "daily", want: "0 3 * * *"},
		{interval: "monthly", want: "0 3 1 * *"},
		{interval: "hourly", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.interval, func(t *testing.T) {
			got, err := refreshCron(tt.interval)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error for unsupported refresh interval")
				}
				return
			}
			if err != nil {
				t.Fatalf("refreshCron() returned an error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("refreshCron() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSongsForGigFiltersByExactArtistAndDate(t *testing.T) {
	date, _ := time.Parse("2006-01-02", "2025-06-14")
	gig := Gig{Date: "2025-06-14", Band: "The Example"}
	setlists := []Setlist{
		{
			Artist:    setlistArtist{Name: "the example"},
			EventDate: "14-06-2025",
			Sets: []setlistSet{{Songs: []setlistSong{
				{Name: " First Song "},
				{Name: "Second Song"},
			}}},
		},
		{
			Artist:    setlistArtist{Name: "The Example"},
			EventDate: "15-06-2025",
			Sets:      []setlistSet{{Songs: []setlistSong{{Name: "Wrong Date"}}}},
		},
		{
			Artist:    setlistArtist{Name: "Another Band"},
			EventDate: "14-06-2025",
			Sets:      []setlistSet{{Songs: []setlistSong{{Name: "Wrong Band"}}}},
		},
	}

	songs := songsForGig(gig, date, setlists)
	if len(songs) != 2 {
		t.Fatalf("expected 2 songs, got %d", len(songs))
	}
	if songs[0].Name != "First Song" || songs[1].Name != "Second Song" {
		t.Fatalf("unexpected songs: %#v", songs)
	}
}

func TestSongsFromSetlistUsesProfileArtistAndSetSongs(t *testing.T) {
	setlist := Setlist{
		Artist:    setlistArtist{Name: " The Example "},
		EventDate: "14-06-2025",
		Sets: []setlistSet{
			{Songs: []setlistSong{{Name: " First Song "}, {Name: ""}}},
			{Songs: []setlistSong{{Name: "Encore"}}},
		},
	}
	songs := songsFromSetlist(setlist)
	if len(songs) != 2 {
		t.Fatalf("expected 2 songs, got %d", len(songs))
	}
	if songs[0].Name != "First Song" || songs[1].Name != "Encore" {
		t.Fatalf("unexpected songs: %#v", songs)
	}
	if len(songs[0].Artists) != 1 || songs[0].Artists[0].Name != "The Example" {
		t.Fatalf("setlist artist was not used for matching: %#v", songs[0].Artists)
	}
}

func TestCountSetlistSongs(t *testing.T) {
	setlist := Setlist{
		Sets: []setlistSet{
			{Songs: []setlistSong{{Name: "First"}, {Name: " "}, {Name: "Second"}}},
			{Songs: []setlistSong{{Name: "Encore"}}},
		},
	}
	if got := countSetlistSongs(setlist); got != 4 {
		t.Fatalf("countSetlistSongs() = %d, want 4", got)
	}
	if got := countNamedSetlistSongs(setlist); got != 3 {
		t.Fatalf("countNamedSetlistSongs() = %d, want 3", got)
	}
}

func TestSetlistResponseUsesSetAndSongJSONFields(t *testing.T) {
	response, err := decodeSetlistResponse([]byte(`{
		"setlist": [{
			"artist": {"name": "The Example"},
			"eventDate": "14-06-2025",
			"set": [{"song": [{"name": "First Song"}]}]
		}],
		"total": 1,
		"page": 1,
		"itemsPerPage": 20
	}`))
	if err != nil {
		t.Fatalf("decode setlist response: %v", err)
	}
	if len(response.Setlists) != 1 || len(response.Setlists[0].Sets) != 1 || len(response.Setlists[0].Sets[0].Songs) != 1 {
		t.Fatalf("setlist response fields were not decoded: %#v", response)
	}
	if response.Setlists[0].Sets[0].Songs[0].Name != "First Song" {
		t.Fatalf("unexpected decoded song: %#v", response.Setlists[0].Sets[0].Songs[0])
	}
}

func TestSetlistResponseUsesWrappedSetsFromAPI(t *testing.T) {
	response, err := decodeSetlistResponse([]byte(`{
		"setlist": [{
			"id": "3aa4127",
			"artist": {"name": "Slipknot"},
			"eventDate": "18-12-2024",
			"sets": {
				"set": [
					{"song": [{"name": "Dream Weaver"}, {"name": "(sic)"}]},
					{"encore": 1, "song": [{"name": "Surfacing"}]}
				]
			}
		}],
		"total": 1,
		"page": 1,
		"itemsPerPage": 20
	}`))
	if err != nil {
		t.Fatalf("decode setlist response with wrapped sets: %v", err)
	}
	if len(response.Setlists) != 1 {
		t.Fatalf("expected 1 setlist, got %d", len(response.Setlists))
	}
	setlist := response.Setlists[0]
	if setlist.ID != "3aa4127" || setlist.Artist.Name != "Slipknot" || len(setlist.Sets) != 2 {
		t.Fatalf("setlist fields were not decoded: %#v", setlist)
	}
	songs := songsFromSetlist(setlist)
	if len(songs) != 3 || songs[0].Name != "Dream Weaver" || songs[2].Name != "Surfacing" {
		t.Fatalf("wrapped song entries were not extracted: %#v", songs)
	}
}

func TestDecodeSetlistResponseReportsUnexpectedBody(t *testing.T) {
	_, err := decodeSetlistResponse([]byte(`{"error":"invalid api key"}`))
	if err == nil {
		t.Fatal("expected an error for a response without a setlist field")
	}
	if !strings.Contains(err.Error(), "did not contain a setlist result field") || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("unexpected diagnostic error: %v", err)
	}
}

func TestResponseBodySummaryTruncatesLongBodies(t *testing.T) {
	body := strings.Repeat("x", 500)
	summary := responseBodySummary([]byte(body))
	if len(summary) != 403 || !strings.HasSuffix(summary, "...") {
		t.Fatalf("unexpected summary length or suffix: %d, %q", len(summary), summary[len(summary)-3:])
	}
}

func TestResponseHeaderLookupIsCaseInsensitive(t *testing.T) {
	got := responseHeader(map[string]string{"retry-after": "30"}, "Retry-After")
	if got != "30" {
		t.Fatalf("responseHeader() = %q, want %q", got, "30")
	}
}

func TestUserConfigurationsKeepSettingsSeparate(t *testing.T) {
	raw := `[
		{"username":"alice","apiKey":"alice-key","playlistName":"Alice Live","importMode":"profile","liveVersionPreference":"preferLive","minimumTrackLengthSeconds":45,"setlistUserId":"alice-id"},
		{"username":"bob","apiKey":"bob-key","importMode":"manual","gigs":[{"date":"2025-07-20","band":"Another Band"}]}
	]`
	var configs []userConfig
	if err := json.Unmarshal([]byte(raw), &configs); err != nil {
		t.Fatalf("decode per-user configuration: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 user configurations, got %d", len(configs))
	}
	if configs[0].Username != "alice" || configs[0].APIKey != "alice-key" || configs[0].SetlistUserID != "alice-id" || configs[0].ImportMode != importModeProfile || configs[0].LivePreference != liveVersionPrefer || minimumTrackLength(configs[0]) != 45 {
		t.Fatalf("Alice's settings were not decoded independently: %#v", configs[0])
	}
	if configs[1].Username != "bob" || configs[1].APIKey != "bob-key" || configs[1].PlaylistName != "" || configs[1].SetlistUserID != "" || configs[1].ImportMode != importModeManual || configs[1].Gigs[0].Band != "Another Band" {
		t.Fatalf("Bob's settings were not decoded independently: %#v", configs[1])
	}
}

func TestValidateImportSource(t *testing.T) {
	tests := []struct {
		name string
		cfg  userConfig
		mode string
		want string
	}{
		{
			name: "manual source",
			cfg:  userConfig{ImportMode: importModeManual, Gigs: []Gig{{Date: "2025-06-14", Band: "The Example"}}, SetlistUserID: "ignored-id"},
			mode: importModeManual,
		},
		{
			name: "profile source",
			cfg:  userConfig{ImportMode: importModeProfile, SetlistUserID: "profile-id", Gigs: []Gig{{Date: "invalid", Band: ""}}},
			mode: importModeProfile,
		},
		{
			name: "legacy profile config",
			cfg:  userConfig{SetlistUserID: "profile-id"},
			mode: importModeProfile,
		},
		{
			name: "legacy manual config",
			cfg:  userConfig{Gigs: []Gig{{Date: "2025-06-14", Band: "The Example"}}},
			mode: importModeManual,
		},
		{
			name: "manual source requires gigs",
			cfg:  userConfig{ImportMode: importModeManual},
			want: "manual mode requires at least one gig",
		},
		{
			name: "profile source requires ID",
			cfg:  userConfig{ImportMode: importModeProfile},
			want: "setlist.fm profile mode requires a profile user ID",
		},
		{
			name: "profile source requires an ID rather than URL",
			cfg:  userConfig{ImportMode: importModeProfile, SetlistUserID: "https://www.setlist.fm/user/alice"},
			want: "setlist.fm user ID must be the profile ID, not a full URL",
		},
		{
			name: "invalid source",
			cfg:  userConfig{ImportMode: "both", Gigs: []Gig{{Date: "2025-06-14", Band: "The Example"}}},
			want: `unknown import mode "both"; choose "manual" or "profile"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateImportSource(&tt.cfg)
			if tt.want != "" {
				if err == nil || err.Error() != tt.want {
					t.Fatalf("validateImportSource() error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateImportSource() returned an error: %v", err)
			}
			if tt.cfg.ImportMode != tt.mode {
				t.Fatalf("resolved import mode = %q, want %q", tt.cfg.ImportMode, tt.mode)
			}
		})
	}
}

func TestValidateLivePreference(t *testing.T) {
	for _, preference := range []string{"", liveVersionAny, liveVersionPrefer, liveVersionAvoid} {
		cfg := userConfig{LivePreference: preference}
		if err := validateLivePreference(&cfg); err != nil {
			t.Errorf("validateLivePreference(%q) returned an error: %v", preference, err)
		}
		if cfg.LivePreference == "" {
			t.Errorf("empty preference should default to %q", liveVersionAny)
		}
	}
	if err := validateLivePreference(&userConfig{LivePreference: "sometimes"}); err == nil {
		t.Fatal("expected an error for an unsupported live preference")
	}
}

func TestMinimumTrackLengthDefaultsToOneMinuteAndCanBeDisabled(t *testing.T) {
	if got := minimumTrackLength(userConfig{}); got != 60 {
		t.Fatalf("minimumTrackLength() = %d, want default 60 seconds", got)
	}

	zero := 0
	if got := minimumTrackLength(userConfig{MinTrackLength: &zero}); got != 0 {
		t.Fatalf("minimumTrackLength() with zero setting = %d, want 0", got)
	}

	short := types.Track{Duration: 59.99}
	exact := types.Track{Duration: 60}
	if !isTrackBelowMinLength(short, userConfig{}) {
		t.Fatal("track shorter than one minute should be filtered")
	}
	if isTrackBelowMinLength(exact, userConfig{}) {
		t.Fatal("track exactly one minute long should be kept")
	}
	if isTrackBelowMinLength(short, userConfig{MinTrackLength: &zero}) {
		t.Fatal("zero minimum should disable the filter")
	}
}

func TestSearchResult3IgnoresStructuredGenres(t *testing.T) {
	raw := `{
		"song": [{
			"id": "track-1",
			"title": "Ten Thousand Fists",
			"artist": "Disturbed",
			"album": "Ten Thousand Fists",
			"duration": 224,
			"genres": [{"name": "Metal", "songCount": 1, "albumCount": 1}]
		}]
	}`

	var result songSearchResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode search3 result with structured genres: %v", err)
	}
	if len(result.Songs) != 1 {
		t.Fatalf("expected one search result, got %d", len(result.Songs))
	}
	track := result.Songs[0]
	if track.ID != "track-1" || track.Title != "Ten Thousand Fists" || track.Artist != "Disturbed" || track.Duration != 224 {
		t.Fatalf("search result fields were not decoded: %#v", track)
	}
}

func TestLiveVersionDetection(t *testing.T) {
	tests := []struct {
		name  string
		track types.Track
		want  bool
	}{
		{name: "live album", track: types.Track{Album: "Live at Wembley"}, want: true},
		{name: "unplugged album", track: types.Track{Album: "MTV Unplugged"}, want: true},
		{name: "live path", track: types.Track{Path: "Artist/Live Concert/Track.flac"}, want: true},
		{name: "live title suffix", track: types.Track{Title: "Song (Live)"}, want: true},
		{name: "normal album", track: types.Track{Album: "Studio Sessions", Title: "Song"}, want: false},
		{name: "phrase is not a live marker", track: types.Track{Title: "Live and Let Die", Album: "Wings Greatest"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLiveVersion(tt.track); got != tt.want {
				t.Errorf("isLiveVersion() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestChooseVersionCandidateUsesExactArtistAndTitle(t *testing.T) {
	song := types.SongRef{Name: "Song", Artists: []types.ArtistRef{{Name: "The Example"}}}
	candidates := []types.Track{
		{ID: "wrong-title", Title: "Song Live and Let Die", Artist: "The Example", Album: "Live"},
		{ID: "wrong-artist", Title: "Song", Artist: "Another Band", Album: "Live at Wembley"},
		{ID: "studio", Title: "Song", Artist: "The Example", Album: "Studio Album"},
		{ID: "live", Title: "Song (Live at Wembley)", Artist: "The Example", Album: "Live at Wembley"},
	}

	if got := chooseVersionCandidate(song, candidates, liveVersionPrefer); got == nil || got.ID != "live" {
		t.Fatalf("prefer-live candidate = %#v, want live track", got)
	}
	if got := chooseVersionCandidate(song, candidates, liveVersionAvoid); got == nil || got.ID != "studio" {
		t.Fatalf("avoid-live candidate = %#v, want studio track", got)
	}
}

func TestUniqueIDsPreservesExistingOrderAndRemovesDuplicates(t *testing.T) {
	got := uniqueIDs([]string{"old-1", "old-2", "old-1"}, []string{"new-1", "old-2", ""})
	want := []string{"old-1", "old-2", "new-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uniqueIDs() = %#v, want %#v", got, want)
	}
}
