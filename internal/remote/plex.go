package remote

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juanvillacortac/audiofilus/internal/config"
	"github.com/juanvillacortac/audiofilus/internal/version"
)

// Plex: the account signs in on plex.tv (a code on plex.tv/link, or login and
// password) and gets a token. plex.tv lists the account's servers with their
// current addresses and their own token (a shared server has a different
// one). Each Audiofilus server entry is one music library (section) of one
// Plex server; its songs are the section's tracks (type 10).

// plexTV is plex.tv's address; a var so the tests can use a fake.
var plexTV = "https://plex.tv"

// plexPage is how many tracks each catalog page asks for; a var for the tests.
var plexPage = 1000

// plexHTTP is for plex.tv and the short requests (connection tests).
var plexHTTP = &http.Client{Timeout: 20 * time.Second}

// plexIdentifier is what Plex calls a library item in /:/rate and /:/scrobble.
const plexIdentifier = "com.plexapp.plugins.library"

// ErrPlex2FA means the account signs in with two factors: PlexSignIn has to
// be called again with the code from the authenticator app.
var ErrPlex2FA = errors.New("plex: verification code required")

// plexClientID identifies this device to Plex. It is fixed per machine, so
// the account's device list shows one Audiofilus per console, not a new one
// each start.
func plexClientID() string {
	host, _ := os.Hostname()
	sum := sha1.Sum([]byte("audiofilus|plex|" + host))
	return "audiofilus-" + hex.EncodeToString(sum[:8])
}

// plexDo sends a request with Plex's headers (and form as the body, if any)
// and decodes the JSON answer into out. It returns the response headers, for
// the server's Date.
func plexDo(ctx context.Context, cl *http.Client, method, u, token string, form url.Values, out any) (http.Header, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Accept", "application/json")
	h.Set("X-Plex-Client-Identifier", plexClientID())
	h.Set("X-Plex-Product", "Audiofilus")
	h.Set("X-Plex-Version", version.Version)
	h.Set("X-Plex-Platform", runtime.GOOS)
	h.Set("X-Plex-Device-Name", "Audiofilus")
	if token != "" {
		h.Set("X-Plex-Token", token)
	}
	if form != nil {
		h.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, netErr("plex: "+req.URL.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// plex.tv explains its errors: {"errors":[{"code":1029,"message":"..."}]}.
		var e struct {
			Errors []struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"errors"`
		}
		if json.NewDecoder(resp.Body).Decode(&e) == nil && len(e.Errors) > 0 {
			if e.Errors[0].Code == 1029 {
				return nil, ErrPlex2FA
			}
			return nil, fmt.Errorf("plex: %s", e.Errors[0].Message)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, errors.New("plex: not signed in (the sign-in was revoked?)")
		}
		return nil, fmt.Errorf("plex: %s %s", req.URL.Path, resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return nil, fmt.Errorf("plex: %s: %w", req.URL.Path, err)
		}
	}
	return resp.Header, nil
}

// PlexPin is a sign-in code to enter on plex.tv/link.
type PlexPin struct {
	ID   int    `json:"id"`
	Code string `json:"code"`
}

// NewPlexPin asks plex.tv for a code (four characters, for plex.tv/link).
func NewPlexPin(ctx context.Context) (PlexPin, error) {
	var p PlexPin
	_, err := plexDo(ctx, plexHTTP, "POST", plexTV+"/api/v2/pins", "", nil, &p)
	return p, err
}

// Token is the account token once the code was entered on plex.tv/link, and
// "" until then.
func (p PlexPin) Token(ctx context.Context) (string, error) {
	var r struct {
		AuthToken string `json:"authToken"`
	}
	_, err := plexDo(ctx, plexHTTP, "GET", fmt.Sprintf("%s/api/v2/pins/%d", plexTV, p.ID), "", nil, &r)
	return r.AuthToken, err
}

// PlexSignIn signs in with login and password (code is the two-factor code,
// "" without it) and returns the account token. The password goes in the
// body, not in the URL.
func PlexSignIn(ctx context.Context, login, password, code string) (string, error) {
	f := url.Values{"login": {login}, "password": {password}, "rememberMe": {"true"}}
	if code != "" {
		f.Set("verificationCode", code)
	}
	var r struct {
		AuthToken string `json:"authToken"`
	}
	_, err := plexDo(ctx, plexHTTP, "POST", plexTV+"/api/v2/users/signin", "", f, &r)
	return r.AuthToken, err
}

// PlexServer is a Plex server the account can use.
type PlexServer struct {
	Name        string `json:"name"`
	Machine     string `json:"clientIdentifier"`
	Provides    string `json:"provides"`
	Token       string `json:"accessToken"`
	Connections []struct {
		URI   string `json:"uri"`
		Local bool   `json:"local"`
		Relay bool   `json:"relay"`
	} `json:"connections"`
}

// PlexServers are the account's servers (its own and the shared ones).
func PlexServers(ctx context.Context, token string) ([]PlexServer, error) {
	var all []PlexServer
	if _, err := plexDo(ctx, plexHTTP, "GET", plexTV+"/api/v2/resources?includeHttps=1&includeRelay=1", token, nil, &all); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(s PlexServer) bool { return !strings.Contains(s.Provides, "server") }), nil
}

// Connect tries all the server's addresses at once and returns the best one
// that answers: on the LAN, then remote, then through Plex's relay. It
// stops as soon as a LAN address answers.
func (s PlexServer) Connect(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rank := func(i int) int {
		switch c := s.Connections[i]; {
		case c.Relay:
			return 2
		case c.Local:
			return 0
		}
		return 1
	}
	type result struct {
		i  int
		ok bool
	}
	ch := make(chan result, len(s.Connections))
	for i, c := range s.Connections {
		go func() {
			_, err := plexDo(ctx, plexHTTP, "GET", c.URI+"/identity", s.Token, nil, nil)
			ch <- result{i, err == nil}
		}()
	}
	best := -1
	for range s.Connections {
		r := <-ch
		if r.ok && (best < 0 || rank(r.i) < rank(best)) {
			best = r.i
		}
		if best >= 0 && rank(best) == 0 {
			break
		}
	}
	if best < 0 {
		return "", fmt.Errorf("plex: %s doesn't answer", s.Name)
	}
	return s.Connections[best].URI, nil
}

// PlexSection is a library of a Plex server.
type PlexSection struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// PlexSections are the server's music libraries.
func PlexSections(ctx context.Context, base, token string) ([]PlexSection, error) {
	var r struct {
		MediaContainer struct {
			Directory []PlexSection `json:"Directory"`
		}
	}
	if _, err := plexDo(ctx, plexHTTP, "GET", strings.TrimRight(base, "/")+"/library/sections", token, nil, &r); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(r.MediaContainer.Directory, func(s PlexSection) bool { return s.Type != "artist" }), nil
}

// plex is the Backend of one music library of a Plex server.
type plex struct {
	srv  config.Server
	http *http.Client

	mu     sync.Mutex
	base   string    // the address that answered (srv.URL until Ping)
	token  string    // the server's token (srv.Password until Ping)
	pinged bool      // Ping already looked the server up
	date   time.Time // the server's time in its last answer (Date header)
}

func newPlex(srv config.Server) *plex {
	return &plex{srv: srv, http: &http.Client{Timeout: 60 * time.Second},
		base: strings.TrimRight(srv.URL, "/"), token: srv.Password}
}

// Ping looks the server up on plex.tv for its best address and its token
// (keeping the saved ones if plex.tv can't be reached) and checks that the
// library answers.
func (p *plex) Ping(ctx context.Context) error {
	base, token := strings.TrimRight(p.srv.URL, "/"), p.srv.Password
	servers, _ := PlexServers(ctx, p.srv.Password)
	for _, s := range servers {
		if s.Machine != p.srv.Machine {
			continue
		}
		token = cmp.Or(s.Token, token)
		if u, err := s.Connect(ctx); err == nil {
			base = u
		}
	}
	if base == "" {
		return fmt.Errorf("plex: the server isn't on this account")
	}
	p.mu.Lock()
	p.base, p.token, p.pinged = base, token, true
	p.mu.Unlock()
	// The library's own entry: counting its tracks took 1.5 s on a big one.
	return p.get(ctx, "GET", "/library/sections/"+url.PathEscape(p.srv.Section), nil)
}

// where is the server's address and token.
func (p *plex) where() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.base, p.token
}

// get makes a request to the server (looking it up first, once) and
// decodes the answer into out.
func (p *plex) get(ctx context.Context, method, path string, out any) error {
	p.mu.Lock()
	pinged := p.pinged
	p.mu.Unlock()
	if !pinged {
		if err := p.Ping(ctx); err != nil {
			return err
		}
	}
	base, token := p.where()
	h, err := plexDo(ctx, p.http, method, base+path, token, nil, out)
	if d, e := http.ParseTime(h.Get("Date")); e == nil {
		p.mu.Lock()
		p.date = d
		p.mu.Unlock()
	}
	return err
}

// plexTrack is a track as Plex lists it.
type plexTrack struct {
	RatingKey        string  `json:"ratingKey"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"originalTitle"` // the track's artist, if not the album's
	GrandparentTitle string  `json:"grandparentTitle"`
	ParentTitle      string  `json:"parentTitle"`
	ParentRatingKey  string  `json:"parentRatingKey"`
	ParentThumb      string  `json:"parentThumb"`
	Thumb            string  `json:"thumb"`
	Index            int     `json:"index"`
	ParentIndex      int     `json:"parentIndex"`
	Duration         int     `json:"duration"` // ms
	UserRating       float64 `json:"userRating"`
	Section          int     `json:"librarySectionID"`
	Media            []struct {
		Container string `json:"container"`
		Part      []struct {
			Key string `json:"key"` // /library/parts/<id>/<timestamp>/file.flac
		} `json:"Part"`
	} `json:"Media"`
}

// id is the track's id in Audiofilus: its ratingKey, part id and part
// timestamp joined by "_", so DownloadURL can build the file's address
// without asking the server.
func (t plexTrack) id() string {
	if len(t.Media) > 0 && len(t.Media[0].Part) > 0 {
		f := strings.Split(strings.TrimPrefix(t.Media[0].Part[0].Key, "/library/parts/"), "/")
		if len(f) >= 3 {
			return t.RatingKey + "_" + f[0] + "_" + f[1]
		}
	}
	return t.RatingKey
}

// splitPlexID undoes id: the ratingKey and the file's path on the server.
func splitPlexID(id string) (key, file string) {
	key, rest, _ := strings.Cut(id, "_")
	part, ts, _ := strings.Cut(rest, "_")
	return key, "/library/parts/" + part + "/" + ts + "/file"
}

// song turns a track into a Song. Plex rates 0 to 10; a favorite is 10
// (Plexamp's "love").
func (t plexTrack) song() Song {
	s := Song{ID: t.id(), Title: t.Title, Artist: cmp.Or(t.OriginalTitle, t.GrandparentTitle), AlbumArtist: t.GrandparentTitle,
		Album: t.ParentTitle, AlbumID: t.ParentRatingKey, Track: t.Index, Disc: t.ParentIndex, Duration: t.Duration / 1000,
		Rating: int(math.Round(t.UserRating / 2))}
	if t.UserRating >= 10 {
		s.Starred = "1"
	}
	if len(t.Media) > 0 {
		s.Suffix = t.Media[0].Container
	}
	switch {
	case t.ParentThumb != "" && t.ParentRatingKey != "":
		s.CoverArt = t.ParentRatingKey
	case t.Thumb != "":
		s.CoverArt = t.RatingKey
	}
	return s
}

// tracks asks for a page of the library's tracks, newest first, and the
// total. filter is a raw Plex filter ("updatedAt>>=123"), kept out of
// url.Values so its ">>=" isn't escaped. size 0 asks only for the total.
func (p *plex) tracks(ctx context.Context, filter string, start, size int) ([]plexTrack, int, error) {
	q := url.Values{"type": {"10"}, "sort": {"addedAt:desc"},
		"X-Plex-Container-Start": {strconv.Itoa(start)}, "X-Plex-Container-Size": {strconv.Itoa(size)}}
	path := "/library/sections/" + url.PathEscape(p.srv.Section) + "/all?" + q.Encode()
	if filter != "" {
		path += "&" + filter
	}
	var r struct {
		MediaContainer struct {
			TotalSize int         `json:"totalSize"`
			Metadata  []plexTrack `json:"Metadata"`
		}
	}
	err := p.get(ctx, "GET", path, &r)
	return r.MediaContainer.Metadata, r.MediaContainer.TotalSize, err
}

// AllSongs is the whole library: the first page brings the total and the
// rest come in parallel.
func (p *plex) AllSongs(ctx context.Context, progress func(Progress)) ([]Song, error) {
	first, total, err := p.tracks(ctx, "", 0, plexPage)
	if err != nil {
		return nil, err
	}
	return allPages(ctx, first, total, plexPage, func(ctx context.Context, n int) ([]plexTrack, error) {
		t, _, err := p.tracks(ctx, "", n*plexPage, plexPage)
		return t, err
	}, progress)
}

// every is all the tracks that match filter.
func (p *plex) every(ctx context.Context, filter string) ([]plexTrack, error) {
	var out []plexTrack
	for start := 0; ; start += plexPage {
		t, _, err := p.tracks(ctx, filter, start, plexPage)
		if err != nil {
			return nil, err
		}
		out = append(out, t...)
		if len(t) < plexPage {
			return out, nil
		}
	}
}

// count is how many tracks there are, and the server's time when it said so.
func (p *plex) count(ctx context.Context) (int, time.Time, error) {
	_, total, err := p.tracks(ctx, "", 0, 0)
	p.mu.Lock()
	at := p.date
	p.mu.Unlock()
	if at.IsZero() {
		at = time.Now()
	}
	return total, at, err
}

// SyncPoint is the server's time before a full load (see Syncer).
func (p *plex) SyncPoint(ctx context.Context) (time.Time, error) {
	_, at, err := p.count(ctx)
	return at, err
}

// Changed is what changed since since (see Syncer). Rating a track doesn't
// touch its updatedAt, so the tracks rated since then are asked for apart,
// and the favorites (rated 10) every time.
func (p *plex) Changed(ctx context.Context, since time.Time) (Changes, error) {
	var ch Changes
	var err error
	if ch.Total, ch.At, err = p.count(ctx); err != nil {
		return ch, err
	}
	for _, f := range []string{"updatedAt>>=", "lastRatedAt>>="} {
		t, err := p.every(ctx, f+strconv.FormatInt(since.Unix()-1, 10))
		if err != nil {
			return ch, err
		}
		for _, t := range t {
			ch.Songs = append(ch.Songs, t.song())
		}
	}
	favs, err := p.every(ctx, "userRating>>=9")
	if err != nil {
		return ch, err
	}
	ch.Favorites = make(map[string]bool, len(favs))
	for _, t := range favs {
		ch.Favorites[t.id()] = true
	}
	return ch, nil
}

// Playlists are the account's audio playlists, with only the songs of this
// library (a Plex playlist can mix libraries); the ones left empty are skipped.
func (p *plex) Playlists(ctx context.Context) ([]Playlist, error) {
	var r struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
				Title     string `json:"title"`
			} `json:"Metadata"`
		}
	}
	if err := p.get(ctx, "GET", "/playlists?playlistType=audio", &r); err != nil {
		return nil, err
	}
	var out []Playlist
	for _, l := range r.MediaContainer.Metadata {
		var items struct {
			MediaContainer struct {
				Metadata []plexTrack `json:"Metadata"`
			}
		}
		if err := p.get(ctx, "GET", "/playlists/"+url.PathEscape(l.RatingKey)+"/items", &items); err != nil {
			continue
		}
		pl := Playlist{ID: l.RatingKey, Name: l.Title}
		for _, t := range items.MediaContainer.Metadata {
			if strconv.Itoa(t.Section) == p.srv.Section {
				pl.Songs = append(pl.Songs, t.id())
			}
		}
		if len(pl.Songs) > 0 {
			out = append(out, pl)
		}
	}
	return out, nil
}

// StreamURL is the file as it is by default (like Plexamp's "Maximum":
// never convert), or MP3 from Plex's transcoder at the quality chosen.
func (p *plex) StreamURL(id string) string {
	if p.srv.Quality == "" || p.srv.Quality == "original" {
		return p.DownloadURL(id)
	}
	base, token := p.where()
	key, _ := splitPlexID(id)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	session := hex.EncodeToString(b)
	v := url.Values{"path": {"/library/metadata/" + key}, "protocol": {"http"}, "musicBitrate": {cmp.Or(p.srv.Quality, "192")},
		"directPlay": {"0"}, "directStream": {"0"}, "session": {session}, "X-Plex-Session-Identifier": {session},
		"X-Plex-Client-Identifier": {plexClientID()}, "X-Plex-Product": {"Audiofilus"}, "X-Plex-Platform": {"Generic"}, // the transcoder has no profile for "windows" or "linux" and answers 400
		"X-Plex-Client-Profile-Extra": {"add-transcode-target(replace=true&type=musicProfile&context=streaming&protocol=http&container=mp3&audioCodec=mp3)"},
		"X-Plex-Token":                {token}}
	return base + "/audio/:/transcode/universal/start?" + v.Encode()
}

// DownloadURL is the original file.
func (p *plex) DownloadURL(id string) string {
	base, token := p.where()
	_, file := splitPlexID(id)
	return base + file + "?download=1&X-Plex-Token=" + url.QueryEscape(token)
}

// CoverURL is the cover of the album (or track) key at size pixels.
func (p *plex) CoverURL(key string, size int) string {
	base, token := p.where()
	n := strconv.Itoa(size)
	v := url.Values{"url": {"/library/metadata/" + key + "/thumb"}, "width": {n}, "height": {n},
		"minSize": {"1"}, "upscale": {"1"}, "X-Plex-Token": {token}}
	return base + "/photo/:/transcode?" + v.Encode()
}

// plexTranscoding lets one transcoded download run at a time: when the next
// song's prefetch starts a second transcode, Plex cuts one of them short
// (unexpected EOF). It is global because the Store can make two clients of
// the same server at once.
var plexTranscoding sync.Mutex

// fetch downloads u; a transcoded one waits for the one before to finish and
// frees its turn when its body is closed.
func (p *plex) fetch(ctx context.Context, u string) (*http.Response, error) {
	if !strings.Contains(u, "/transcode/universal/") {
		return getFile(ctx, u)
	}
	plexTranscoding.Lock()
	resp, err := getFile(ctx, u)
	if err != nil {
		plexTranscoding.Unlock()
		return nil, err
	}
	resp.Body = &unlockBody{ReadCloser: resp.Body, unlock: sync.OnceFunc(plexTranscoding.Unlock)}
	return resp, nil
}

// unlockBody is a response body that calls unlock when closed.
type unlockBody struct {
	io.ReadCloser
	unlock func()
}

func (b *unlockBody) Close() error {
	defer b.unlock()
	return b.ReadCloser.Close()
}
