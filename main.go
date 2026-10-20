package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/lifecycle"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/navidrome/navidrome/plugins/pdk/go/scheduler"
	"github.com/navidrome/navidrome/plugins/pdk/go/types"
)

const (
	defaultPlaylistName    = "Live Setlists"
	setlistAPIBase         = "https://api.setlist.fm/rest/1.0/search/setlists"
	importModeManual       = "manual"
	importModeProfile      = "profile"
	setlistRequestDelay    = 2 * time.Second
	defaultRefreshInterval = "weekly"
	defaultMinTrackLength  = 60
	liveVersionAny         = "any"
	liveVersionPrefer      = "preferLive"
	liveVersionAvoid       = "avoidLive"
	liveVersionSearchLimit = 100
)

var (
	setlistRequestMu   sync.Mutex
	lastSetlistRequest time.Time
)

type userConfig struct {
	APIKey         string `json:"apiKey"`
	Username       string `json:"username"`
	PlaylistName   string `json:"playlistName"`
	ImportMode     string `json:"importMode"`
	LivePreference string `json:"liveVersionPreference"`
	MinTrackLength *int   `json:"minimumTrackLengthSeconds"`
	SetlistUserID  string `json:"setlistUserId"`
	Gigs           []Gig  `json:"gigs"`
}

type Gig struct {
	Date string `json:"date"`
	Band string `json:"band"`
}

type setlistResponse struct {
	Setlists []Setlist `json:"setlist"`
	Total    int       `json:"total"`
	Page     int       `json:"page"`
	PageSize int       `json:"itemsPerPage"`
}

type Setlist struct {
	Artist    setlistArtist
	EventDate string
	ID        string
	Sets      []setlistSet
}

func (s *Setlist) UnmarshalJSON(data []byte) error {
	var raw struct {
		Artist    setlistArtist   `json:"artist"`
		EventDate string          `json:"eventDate"`
		ID        string          `json:"id"`
		Set       []setlistSet    `json:"set"`
		Sets      json.RawMessage `json:"sets"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Artist = raw.Artist
	s.EventDate = raw.EventDate
	s.ID = raw.ID
	s.Sets = raw.Set
	trimmedSets := strings.TrimSpace(string(raw.Sets))
	if trimmedSets == "" || trimmedSets == "null" {
		return nil
	}

	if strings.HasPrefix(trimmedSets, "{") {
		var wrapped struct {
			Set []setlistSet `json:"set"`
		}
		if err := json.Unmarshal(raw.Sets, &wrapped); err != nil {
			return fmt.Errorf("decode setlist sets: %w", err)
		}
		s.Sets = wrapped.Set
		return nil
	}
	var direct []setlistSet
	if err := json.Unmarshal(raw.Sets, &direct); err != nil {
		return fmt.Errorf("decode setlist sets: %w", err)
	}
	s.Sets = direct
	return nil
}

type setlistArtist struct {
	Name string `json:"name"`
}

type setlistSet struct {
	Songs []setlistSong `json:"song"`
}

type setlistSong struct {
	Name string `json:"name"`
}

type subsonicResponse struct {
	Status        string `json:"status"`
	Error         struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Playlists     struct {
		Playlist []playlistSummary `json:"playlist"`
	} `json:"playlists"`
	Playlist      *playlistDetails `json:"playlist"`
	SearchResult3 *songSearchResult `json:"searchResult3"`
}

type songSearchResult struct {
	Songs []types.Track `json:"song"`
}

func (r *songSearchResult) UnmarshalJSON(data []byte) error {
	var result struct {
		Songs []struct {
			ID       string  `json:"id"`
			Title    string  `json:"title"`
			Artist   string  `json:"artist"`
			Album    string  `json:"album"`
			Path     string  `json:"path"`
			Duration float64 `json:"duration"`
		} `json:"song"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decode searchResult3 songs: %w", err)
	}
	r.Songs = make([]types.Track, 0, len(result.Songs))
	for _, song := range result.Songs {
		r.Songs = append(r.Songs, types.Track{
			ID:       song.ID,
			Title:    song.Title,
			Artist:   song.Artist,
			Album:    song.Album,
			Path:     song.Path,
			Duration: song.Duration,
		})
	}
	return nil
}

type playlistSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type playlistDetails struct {
	Entry []struct {
		ID string `json:"id"`
	} `json:"entry"`
}

type livePlaylistPlugin struct{}

func (p *livePlaylistPlugin) OnInit() error {
	if _, err := loadConfigs(); err != nil {
		return err
	}
	interval, ok := pdk.GetConfig("refreshInterval")
	if !ok {
		interval = defaultRefreshInterval
	}
	cron, err := refreshCron(interval)
	if err != nil {
		return err
	}
	if _, err := host.SchedulerScheduleRecurring(cron, "scheduled-sync", "daily-sync"); err != nil {
		return fmt.Errorf("schedule setlist sync: %w", err)
	}
	if _, err := host.SchedulerScheduleOneTime(1, "startup-sync", ""); err != nil {
		return fmt.Errorf("schedule startup setlist sync: %w", err)
	}
	return nil
}

func refreshCron(interval string) (string, error) {
	switch strings.TrimSpace(interval) {
	case "", "weekly":
		return "0 3 * * 0", nil
	case "daily":
		return "0 3 * * *", nil
	case "monthly":
		return "0 3 1 * *", nil
	default:
		return "", fmt.Errorf("invalid refresh interval %q; choose daily, weekly, or monthly", interval)
	}
}

func (p *livePlaylistPlugin) OnCallback(scheduler.SchedulerCallbackRequest) error {
	configs, err := loadConfigs()
	if err != nil {
		return err
	}
	failures := make([]string, 0)
	for _, cfg := range configs {
		if err := syncPlaylist(cfg); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", cfg.Username, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("one or more user playlist syncs failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func loadConfigs() ([]userConfig, error) {
	usersJSON, ok := pdk.GetConfig("users")
	if !ok {
		return nil, errors.New("missing required 'users' configuration")
	}
	var configs []userConfig
	if err := json.Unmarshal([]byte(usersJSON), &configs); err != nil {
		return nil, fmt.Errorf("invalid users configuration: %w", err)
	}
	if len(configs) == 0 {
		return nil, errors.New("add at least one user configuration")
	}
	seenUsers := make(map[string]struct{}, len(configs))
	for userIndex := range configs {
		cfg := &configs[userIndex]
		cfg.Username = strings.TrimSpace(cfg.Username)
		cfg.APIKey = strings.TrimSpace(cfg.APIKey)
		cfg.PlaylistName = strings.TrimSpace(cfg.PlaylistName)
		cfg.ImportMode = strings.TrimSpace(cfg.ImportMode)
		cfg.LivePreference = strings.TrimSpace(cfg.LivePreference)
		cfg.SetlistUserID = strings.TrimSpace(cfg.SetlistUserID)
		if cfg.Username == "" {
			return nil, fmt.Errorf("user configuration %d is missing a Navidrome username", userIndex+1)
		}
		if _, exists := seenUsers[strings.ToLower(cfg.Username)]; exists {
			return nil, fmt.Errorf("duplicate user configuration for %q", cfg.Username)
		}
		seenUsers[strings.ToLower(cfg.Username)] = struct{}{}
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("user configuration for %q is missing a setlist.fm API key", cfg.Username)
		}
		if cfg.PlaylistName == "" {
			cfg.PlaylistName = defaultPlaylistName
		}
		if err := validateLivePreference(cfg); err != nil {
			return nil, fmt.Errorf("user %q: %w", cfg.Username, err)
		}
		if cfg.MinTrackLength != nil && *cfg.MinTrackLength < 0 {
			return nil, fmt.Errorf("user %q: minimum track length cannot be negative", cfg.Username)
		}
		if err := validateImportSource(cfg); err != nil {
			return nil, fmt.Errorf("user %q: %w", cfg.Username, err)
		}
		if cfg.ImportMode == importModeManual {
			for gigIndex, gig := range cfg.Gigs {
				if strings.TrimSpace(gig.Band) == "" {
					return nil, fmt.Errorf("gig %d for user %q is missing a band name", gigIndex+1, cfg.Username)
				}
				if _, err := parseGigDate(gig.Date); err != nil {
					return nil, fmt.Errorf("gig %d for user %q has an invalid date: %w", gigIndex+1, cfg.Username, err)
				}
			}
		}
	}
	return configs, nil
}

func validateLivePreference(cfg *userConfig) error {
	if cfg.LivePreference == "" {
		cfg.LivePreference = liveVersionAny
	}
	switch cfg.LivePreference {
	case liveVersionAny, liveVersionPrefer, liveVersionAvoid:
		return nil
	default:
		return fmt.Errorf("invalid live version preference %q; choose %q, %q, or %q", cfg.LivePreference, liveVersionAny, liveVersionPrefer, liveVersionAvoid)
	}
}

func validateImportSource(cfg *userConfig) error {
	if cfg.ImportMode == "" {
		if cfg.SetlistUserID != "" && len(cfg.Gigs) == 0 {
			cfg.ImportMode = importModeProfile
		} else {
			cfg.ImportMode = importModeManual
		}
	}
	switch cfg.ImportMode {
	case importModeManual:
		if len(cfg.Gigs) == 0 {
			return errors.New("manual mode requires at least one gig")
		}
	case importModeProfile:
		if cfg.SetlistUserID == "" {
			return errors.New("setlist.fm profile mode requires a profile user ID")
		}
		if strings.ContainsAny(cfg.SetlistUserID, "/?#") {
			return errors.New("setlist.fm user ID must be the profile ID, not a full URL")
		}
	default:
		return fmt.Errorf("unknown import mode %q; choose %q or %q", cfg.ImportMode, importModeManual, importModeProfile)
	}
	return nil
}

func parseGigDate(value string) (time.Time, error) {
	date, err := time.Parse("2006-01-02", value)
	if err != nil || date.Format("2006-01-02") != value {
		return time.Time{}, fmt.Errorf("use YYYY-MM-DD")
	}
	return date, nil
}

func syncPlaylist(cfg userConfig) error {
	var songs []types.SongRef
	seenSongs := make(map[string]struct{})
	addSongs := func(source []types.SongRef) {
		for _, song := range source {
			artist := ""
			if len(song.Artists) > 0 {
				artist = song.Artists[0].Name
			}
			key := strings.ToLower(strings.TrimSpace(artist) + "\x00" + strings.TrimSpace(song.Name))
			if _, exists := seenSongs[key]; exists {
				continue
			}
			seenSongs[key] = struct{}{}
			songs = append(songs, song)
		}
	}
	if cfg.ImportMode == importModeProfile {
		setlists, err := fetchAttendedSetlists(cfg.APIKey, cfg.SetlistUserID)
		if err != nil {
			return fmt.Errorf("fetch attended setlists for setlist.fm user %q: %w", cfg.SetlistUserID, err)
		}
		totalSets, totalSongs, namedSongs, usableSetlists := 0, 0, 0, 0
		emptySetlistExamples := make([]string, 0, 3)
		for _, setlist := range setlists {
			songsFromEvent := songsFromSetlist(setlist)
			totalSets += len(setlist.Sets)
			for _, set := range setlist.Sets {
				totalSongs += len(set.Songs)
			}
			namedSongs += countNamedSetlistSongs(setlist)
			if len(songsFromEvent) > 0 {
				usableSetlists++
			} else if len(emptySetlistExamples) < cap(emptySetlistExamples) {
				emptySetlistExamples = append(emptySetlistExamples, fmt.Sprintf("id=%q artist=%q date=%q sets=%d songEntries=%d namedSongs=%d", setlist.ID, setlist.Artist.Name, setlist.EventDate, len(setlist.Sets), countSetlistSongs(setlist), countNamedSetlistSongs(setlist)))
			}
			addSongs(songsFromEvent)
		}
		pdk.Log(pdk.LogInfo, fmt.Sprintf("setlist.fm profile %q returned %d attended setlists: %d sets, %d song entries (%d with names), %d setlists yielded songs", cfg.SetlistUserID, len(setlists), totalSets, totalSongs, namedSongs, usableSetlists))
		if len(setlists) == 0 {
			pdk.Log(pdk.LogWarn, fmt.Sprintf("No attended setlists found for setlist.fm user %q", cfg.SetlistUserID))
		} else if usableSetlists == 0 {
			pdk.Log(pdk.LogWarn, fmt.Sprintf("Attended setlists yielded no usable song references; examples: %s", strings.Join(emptySetlistExamples, "; ")))
		}
	}
	if cfg.ImportMode == importModeManual {
		for _, gig := range cfg.Gigs {
			date, err := parseGigDate(gig.Date)
			if err != nil {
				return fmt.Errorf("invalid date for %q: %w", gig.Band, err)
			}
			setlists, err := fetchSetlists(cfg.APIKey, gig, date)
			if err != nil {
				return fmt.Errorf("fetch setlist for %q on %s: %w", gig.Band, gig.Date, err)
			}
			gigSongs := songsForGig(gig, date, setlists)
			if len(gigSongs) == 0 && len(setlists) > 0 {
				candidates := make([]string, 0, len(setlists))
				for _, setlist := range setlists {
					candidates = append(candidates, fmt.Sprintf("%q (%s)", setlist.Artist.Name, setlist.EventDate))
				}
				pdk.Log(pdk.LogWarn, fmt.Sprintf("setlist.fm returned %d results for %q on %s, but none matched the exact artist/date filter; results: %s", len(setlists), gig.Band, gig.Date, strings.Join(candidates, ", ")))
			}
			addSongs(gigSongs)
			if len(gigSongs) == 0 {
				pdk.Log(pdk.LogWarn, fmt.Sprintf("No setlist with songs matched %q on %s", gig.Band, gig.Date))
			}
		}
	}
	if len(songs) == 0 {
		pdk.Log(pdk.LogWarn, "No setlist songs were found; playlist was not changed")
		return nil
	}

	matches, err := matchLibrarySongs(cfg, songs)
	if err != nil {
		return fmt.Errorf("match setlist songs against the Navidrome library: %w", err)
	}
	if len(matches) != len(songs) {
		return fmt.Errorf("song matcher returned %d results for %d songs", len(matches), len(songs))
	}
	matchedIDs := make([]string, 0, len(matches))
	for index, match := range matches {
		if match == nil || match.ID == "" {
			artist := ""
			if len(songs[index].Artists) > 0 {
				artist = songs[index].Artists[0].Name
			}
			pdk.Log(pdk.LogWarn, fmt.Sprintf("No library match for %q by %q", songs[index].Name, artist))
			continue
		}
		if isTrackBelowMinLength(*match, cfg) {
			pdk.Log(pdk.LogInfo, fmt.Sprintf("Skipping track %q by %q: duration %.0f seconds is below the %d-second minimum", match.Title, match.Artist, match.Duration, minimumTrackLength(cfg)))
			continue
		}
		matchedIDs = append(matchedIDs, match.ID)
	}
	if len(matchedIDs) == 0 {
		pdk.Log(pdk.LogWarn, "No setlist songs matched the Navidrome library; playlist was not changed")
		return nil
	}

	return appendToPlaylist(cfg, matchedIDs)
}

func minimumTrackLength(cfg userConfig) int {
	if cfg.MinTrackLength == nil {
		return defaultMinTrackLength
	}
	return *cfg.MinTrackLength
}

func isTrackBelowMinLength(track types.Track, cfg userConfig) bool {
	minimum := minimumTrackLength(cfg)
	return minimum > 0 && track.Duration < float64(minimum)
}

func matchLibrarySongs(cfg userConfig, songs []types.SongRef) ([]*types.Track, error) {
	matches, err := host.MatcherMatchSongs(songs, host.MatchOptions{Username: cfg.Username})
	if err != nil {
		return nil, err
	}
	if len(matches) != len(songs) {
		return nil, fmt.Errorf("song matcher returned %d results for %d songs", len(matches), len(songs))
	}
	if cfg.LivePreference == liveVersionAny {
		return matches, nil
	}

	changed, fallback := 0, 0
	for index, song := range songs {
		current := matches[index]
		if current != nil && trackMatchesLivePreference(current, cfg.LivePreference) {
			continue
		}

		candidates, err := searchSongCandidates(cfg.Username, song)
		if err != nil {
			return nil, fmt.Errorf("search library candidates for %q: %w", song.Name, err)
		}
		preferred := chooseVersionCandidate(song, candidates, cfg.LivePreference)
		if preferred == nil {
			fallback++
			continue
		}
		matches[index] = preferred
		if current == nil || preferred.ID != current.ID {
			changed++
		}
	}
	pdk.Log(pdk.LogInfo, fmt.Sprintf("Live-version preference %q selected %d alternate tracks; %d songs used the default match because no preferred version was found", cfg.LivePreference, changed, fallback))
	return matches, nil
}

func searchSongCandidates(username string, song types.SongRef) ([]types.Track, error) {
	artist := songArtist(song)
	if artist == "" || strings.TrimSpace(song.Name) == "" {
		return nil, nil
	}
	params := url.Values{
		"query":       {song.Name + " " + artist},
		"songCount":   {fmt.Sprint(liveVersionSearchLimit)},
		"songOffset":  {"0"},
		"artistCount": {"0"},
		"albumCount":  {"0"},
	}
	response, err := callSubsonic(username, "search3", params)
	if err != nil {
		return nil, err
	}
	if response.SearchResult3 == nil {
		return nil, errors.New("Subsonic search3 response did not include searchResult3")
	}
	return response.SearchResult3.Songs, nil
}

func songArtist(song types.SongRef) string {
	if len(song.Artists) > 0 {
		return strings.TrimSpace(song.Artists[0].Name)
	}
	return strings.TrimSpace(song.Artist)
}

func chooseVersionCandidate(song types.SongRef, candidates []types.Track, preference string) *types.Track {
	wantLive := preference == liveVersionPrefer
	artist := songArtist(song)
	for index := range candidates {
		candidate := &candidates[index]
		if candidate.ID == "" || !strings.EqualFold(strings.TrimSpace(candidate.Artist), artist) {
			continue
		}
		if !sameRecordingTitle(candidate.Title, song.Name) {
			continue
		}
		if isLiveVersion(*candidate) == wantLive {
			return candidate
		}
	}
	return nil
}

func trackMatchesLivePreference(track *types.Track, preference string) bool {
	isLive := isLiveVersion(*track)
	return (preference == liveVersionPrefer && isLive) || (preference == liveVersionAvoid && !isLive)
}

func isLiveVersion(track types.Track) bool {
	if hasLiveMarker(track.Album) || hasLiveMarker(track.Path) || hasLiveMarker(track.Comment) ||
		hasLiveMarker(track.DiscSubtitle) || hasLiveMarker(track.MbzAlbumType) || hasLiveMarker(track.MbzAlbumComment) {
		return true
	}
	for key, values := range track.Tags {
		if strings.Contains(strings.ToLower(key), "album") || strings.Contains(strings.ToLower(key), "release") {
			for _, value := range values {
				if hasLiveMarker(value) {
					return true
				}
			}
		}
	}
	title := strings.ToLower(strings.TrimSpace(track.Title))
	return normalizeRecordingTitle(title) != title
}

func hasLiveMarker(value string) bool {
	for _, word := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		switch word {
		case "live", "unplugged", "concert":
			return true
		}
	}
	return false
}

func sameRecordingTitle(candidate, setlistTitle string) bool {
	candidate = normalizeRecordingTitle(candidate)
	setlistTitle = normalizeRecordingTitle(setlistTitle)
	return candidate != "" && candidate == setlistTitle
}

func normalizeRecordingTitle(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	for {
		if strings.HasSuffix(title, ")") {
			if opening := strings.LastIndex(title, "("); opening > 0 && isLiveQualifier(title[opening+1:len(title)-1]) {
				title = strings.TrimSpace(title[:opening])
				continue
			}
		}
		if strings.HasSuffix(title, "]") {
			if opening := strings.LastIndex(title, "["); opening > 0 && isLiveQualifier(title[opening+1:len(title)-1]) {
				title = strings.TrimSpace(title[:opening])
				continue
			}
		}
		if separator := strings.LastIndex(title, " - "); separator > 0 && isLiveQualifier(title[separator+3:]) {
			title = strings.TrimSpace(title[:separator])
			continue
		}
		break
	}
	return title
}

func isLiveQualifier(value string) bool {
	words := strings.FieldsFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "live", "unplugged", "concert":
		return true
	default:
		return false
	}
}

func fetchSetlists(apiKey string, gig Gig, date time.Time) ([]Setlist, error) {
	query := url.Values{
		"artistName": {gig.Band},
		"date":       {date.Format("02-01-2006")},
	}
	return fetchPaginatedSetlists(apiKey, setlistAPIBase, query)
}

func fetchAttendedSetlists(apiKey, userID string) ([]Setlist, error) {
	endpoint := "https://api.setlist.fm/rest/1.0/user/" + url.PathEscape(userID) + "/attended"
	return fetchPaginatedSetlists(apiKey, endpoint, url.Values{})
}

func fetchPaginatedSetlists(apiKey, endpoint string, query url.Values) ([]Setlist, error) {
	var all []Setlist
	for page := 1; ; page++ {
		pageQuery := url.Values{}
		for key, values := range query {
			pageQuery[key] = append([]string(nil), values...)
		}
		pageQuery.Set("p", fmt.Sprint(page))
		requestURL := endpoint + "?" + pageQuery.Encode()
		response, err := requestSetlistFM(apiKey, fmt.Sprintf("page %d", page), requestURL)
		if err != nil {
			return nil, err
		}
		result, err := decodeSetlistResponse(response.Body)
		if err != nil {
			return nil, err
		}
		pdk.Log(pdk.LogInfo, fmt.Sprintf("setlist.fm page %d returned %d setlists (total %d)", result.Page, len(result.Setlists), result.Total))
		all = append(all, result.Setlists...)
		if len(result.Setlists) == 0 || len(all) >= result.Total {
			break
		}
	}
	return all, nil
}

func requestSetlistFM(apiKey, requestDescription, requestURL string) (*host.HTTPResponse, error) {
	waitForSetlistRequest()
	pdk.Log(pdk.LogInfo, "Requesting setlist.fm "+requestDescription)
	response, err := host.HTTPSend(host.HTTPRequest{
		Method: "GET",
		URL:    requestURL,
		Headers: map[string]string{
			"Accept":    "application/json",
			"x-api-key": apiKey,
		},
		TimeoutMs: 15000,
	})
	if err != nil {
		return nil, fmt.Errorf("request setlist.fm: %w", err)
	}
	if response == nil {
		return nil, errors.New("setlist.fm returned an empty response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryAfter := responseHeader(response.Headers, "Retry-After")
		if retryAfter != "" {
			retryAfter = fmt.Sprintf(" (Retry-After: %s)", retryAfter)
		}
		return nil, fmt.Errorf("setlist.fm returned HTTP %d%s: %s", response.StatusCode, retryAfter, responseBodySummary(response.Body))
	}
	return response, nil
}

func countSetlistSongs(setlist Setlist) int {
	count := 0
	for _, set := range setlist.Sets {
		count += len(set.Songs)
	}
	return count
}

func countNamedSetlistSongs(setlist Setlist) int {
	count := 0
	for _, set := range setlist.Sets {
		for _, song := range set.Songs {
			if strings.TrimSpace(song.Name) != "" {
				count++
			}
		}
	}
	return count
}

func waitForSetlistRequest() {
	setlistRequestMu.Lock()
	defer setlistRequestMu.Unlock()

	if !lastSetlistRequest.IsZero() {
		if wait := setlistRequestDelay - time.Since(lastSetlistRequest); wait > 0 {
			time.Sleep(wait)
		}
	}
	lastSetlistRequest = time.Now()
}

func responseHeader(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func decodeSetlistResponse(body []byte) (setlistResponse, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return setlistResponse{}, fmt.Errorf("decode setlist.fm response: %w (body: %s)", err, responseBodySummary(body))
	}
	if _, ok := fields["setlist"]; !ok {
		return setlistResponse{}, fmt.Errorf("setlist.fm response did not contain a setlist result field (body: %s)", responseBodySummary(body))
	}
	var result setlistResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return setlistResponse{}, fmt.Errorf("decode setlist.fm setlists: %w (body: %s)", err, responseBodySummary(body))
	}
	return result, nil
}

func responseBodySummary(body []byte) string {
	const maxLength = 400
	summary := strings.TrimSpace(string(body))
	if len(summary) > maxLength {
		return summary[:maxLength] + "..."
	}
	if summary == "" {
		return "<empty body>"
	}
	return summary
}

func songsForGig(gig Gig, date time.Time, setlists []Setlist) []types.SongRef {
	songs := make([]types.SongRef, 0)
	for _, setlist := range setlists {
		eventDate, err := time.Parse("02-01-2006", setlist.EventDate)
		if err != nil || !eventDate.Equal(date) || !strings.EqualFold(strings.TrimSpace(setlist.Artist.Name), strings.TrimSpace(gig.Band)) {
			continue
		}
		songs = append(songs, songsFromSetlist(setlist)...)
	}
	return songs
}

func songsFromSetlist(setlist Setlist) []types.SongRef {
	songs := make([]types.SongRef, 0)
	artist := strings.TrimSpace(setlist.Artist.Name)
	if artist == "" {
		return songs
	}
	for _, set := range setlist.Sets {
		for _, song := range set.Songs {
			if strings.TrimSpace(song.Name) == "" {
				continue
			}
			songs = append(songs, types.SongRef{
				Name:    strings.TrimSpace(song.Name),
				Artists: []types.ArtistRef{{Name: artist}},
			})
		}
	}
	return songs
}

func appendToPlaylist(cfg userConfig, newIDs []string) error {
	playlists, err := callSubsonic(cfg.Username, "getPlaylists", url.Values{"username": {cfg.Username}})
	if err != nil {
		return err
	}
	var playlistID string
	for _, playlist := range playlists.Playlists.Playlist {
		if playlist.Name == cfg.PlaylistName {
			playlistID = playlist.ID
			break
		}
	}

	allIDs := make([]string, 0, len(newIDs))
	if playlistID != "" {
		details, err := callSubsonic(cfg.Username, "getPlaylist", url.Values{"id": {playlistID}})
		if err != nil {
			return err
		}
		if details.Playlist == nil {
			return errors.New("Subsonic getPlaylist response did not include playlist details")
		}
		for _, entry := range details.Playlist.Entry {
			if entry.ID != "" {
				allIDs = append(allIDs, entry.ID)
			}
		}
	}
	allIDs = uniqueIDs(allIDs, newIDs)
	if len(allIDs) == 0 {
		pdk.Log(pdk.LogWarn, "No matching songs to add; playlist was not created")
		return nil
	}

	params := url.Values{"songId": allIDs}
	if playlistID == "" {
		params.Set("name", cfg.PlaylistName)
	} else {
		params.Set("playlistId", playlistID)
	}
	if _, err := callSubsonic(cfg.Username, "createPlaylist", params); err != nil {
		return err
	}
	pdk.Log(pdk.LogInfo, fmt.Sprintf("Synced %d songs to playlist %q", len(allIDs), cfg.PlaylistName))
	return nil
}

func callSubsonic(username, method string, params url.Values) (subsonicResponse, error) {
	params.Set("u", username)
	raw, err := host.SubsonicAPICall(fmt.Sprintf("/rest/%s?%s", method, params.Encode()))
	if err != nil {
		return subsonicResponse{}, fmt.Errorf("call Subsonic %s: %w", method, err)
	}
	var envelope struct {
		Response subsonicResponse `json:"subsonic-response"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return subsonicResponse{}, fmt.Errorf("decode Subsonic %s response: %w", method, err)
	}
	if envelope.Response.Status != "ok" {
		return subsonicResponse{}, fmt.Errorf("Subsonic %s failed (%d): %s", method, envelope.Response.Error.Code, envelope.Response.Error.Message)
	}
	return envelope.Response, nil
}

func uniqueIDs(existing, additions []string) []string {
	ids := make([]string, 0, len(existing)+len(additions))
	seen := make(map[string]struct{}, len(existing)+len(additions))
	candidates := make([]string, 0, len(existing)+len(additions))
	candidates = append(candidates, existing...)
	candidates = append(candidates, additions...)
	for _, id := range candidates {
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func main() {}

func init() {
	plugin := &livePlaylistPlugin{}
	lifecycle.Register(plugin)
	scheduler.Register(plugin)
}
