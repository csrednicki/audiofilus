package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juanvillacortac/audiofilus/internal/config"
)

// fakePlex is plex.tv and a Plex server in one: the account token is "acct",
// the server's own token "srv", and its library "3" has n tracks; the ones
// whose number is in rated are rated 10. reqs keeps every server request.
type fakePlex struct {
	*httptest.Server
	n          int
	rated      map[int]bool
	plexTVDown bool

	mu   sync.Mutex
	reqs []*http.Request
}

func newFakePlex(t *testing.T, n int) *fakePlex {
	f := &fakePlex{n: n, rated: map[int]bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Plex-Token")
		if strings.HasPrefix(r.URL.Path, "/api/v2/") {
			f.plexTV(t, w, r)
			return
		}
		if tok == "" {
			tok = r.URL.Query().Get("X-Plex-Token")
		}
		if tok != "srv" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, r)
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/identity", r.URL.Path == "/library/sections/3":
		case r.URL.Path == "/library/sections/3/all":
			q := r.URL.Query()
			start, _ := strconv.Atoi(q.Get("X-Plex-Container-Start"))
			size, _ := strconv.Atoi(q.Get("X-Plex-Container-Size"))
			var items []map[string]any
			for i := start; i < min(start+size, f.n); i++ {
				if q.Has("userRating>>") && !f.rated[i] {
					continue
				}
				items = append(items, f.track(i))
			}
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"totalSize": f.n, "Metadata": items}})
		case r.URL.Path == "/playlists":
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []map[string]any{
				{"ratingKey": "50", "title": "Mixed"}, {"ratingKey": "51", "title": "Other library"}}}})
		case r.URL.Path == "/playlists/50/items":
			a, b := f.track(1), f.track(2)
			b["librarySectionID"] = 4
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []any{a, b}}})
		case r.URL.Path == "/playlists/51/items":
			b := f.track(2)
			b["librarySectionID"] = 4
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []any{b}}})
		case r.URL.Path == "/:/rate", r.URL.Path == "/:/scrobble":
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	old := plexTV
	plexTV = f.URL
	t.Cleanup(func() { plexTV = old })
	return f
}

// plexTV answers what plex.tv does: resources lists the server with a dead
// LAN address and a working remote one, pins and signin hand out "acct".
func (f *fakePlex) plexTV(t *testing.T, w http.ResponseWriter, r *http.Request) {
	switch {
	case f.plexTVDown:
		w.WriteHeader(http.StatusBadGateway)
	case r.URL.Path == "/api/v2/resources":
		if r.Header.Get("X-Plex-Token") != "acct" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"name": "Player", "clientIdentifier": "p1", "provides": "player"},
			{"name": "Home", "clientIdentifier": "m1", "provides": "server", "accessToken": "srv", "connections": []map[string]any{
				{"uri": "http://127.0.0.1:1", "local": true},
				{"uri": f.URL, "local": false},
			}},
		})
	case r.URL.Path == "/api/v2/pins" && r.Method == "POST":
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "code": "K7Q2"})
	case r.URL.Path == "/api/v2/pins/7":
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "authToken": "acct"})
	case r.URL.Path == "/api/v2/users/signin":
		if r.URL.RawQuery != "" {
			t.Errorf("signin sends credentials in the URL: %s", r.URL.RawQuery)
		}
		r.ParseForm()
		if r.PostForm.Get("verificationCode") != "123456" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"errors":[{"code":1029,"message":"Please enter the verification code"}]}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"authToken": "acct"})
	default:
		t.Errorf("unexpected plex.tv %s %s", r.Method, r.URL)
	}
}

// track is track i of library 3: album i/5, its part 100+i.
func (f *fakePlex) track(i int) map[string]any {
	m := map[string]any{"ratingKey": fmt.Sprint(i), "title": fmt.Sprint("t", i), "grandparentTitle": "A",
		"parentTitle": fmt.Sprintf("album %d", i/5), "parentRatingKey": fmt.Sprint(1000 + i/5), "parentThumb": "/x",
		"index": 1 + i%5, "parentIndex": 1, "duration": 61500, "librarySectionID": 3,
		"Media": []map[string]any{{"container": "flac", "Part": []map[string]any{{"key": fmt.Sprintf("/library/parts/%d/1600/file.flac", 100+i)}}}}}
	if f.rated[i] {
		m["userRating"] = 10.0
	}
	return m
}

func plexEntry() config.Server {
	return config.Server{Name: "Home", Type: "plex", Password: "acct", Machine: "m1", Section: "3"}
}

// TestPlexAllSongs loads a library of several pages from a fake server
// found through plex.tv: Ping skips the dead LAN address, uses the server's
// own token, and every track arrives once with its fields mapped.
func TestPlexAllSongs(t *testing.T) {
	old := plexPage
	plexPage = 7
	defer func() { plexPage = old }()
	f := newFakePlex(t, 23)
	f.rated[4] = true
	p := New(plexEntry())
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if base, _ := p.(*plex).where(); base != f.URL {
		t.Errorf("base = %q, want the remote connection %q", base, f.URL)
	}
	songs, err := p.AllSongs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != 23 {
		t.Fatalf("got %d songs, want 23", len(songs))
	}
	var s Song
	for _, x := range songs {
		if x.Title == "t4" {
			s = x
		}
	}
	want := Song{ID: "4_104_1600", Title: "t4", Artist: "A", AlbumArtist: "A", Album: "album 0", AlbumID: "1000",
		Track: 5, Disc: 1, Duration: 61, Suffix: "flac", CoverArt: "1000", Starred: "1", Rating: 5}
	if s != want {
		t.Errorf("song = %+v\nwant %+v", s, want)
	}
}

// TestPlexPingOffline keeps the saved address and token when plex.tv
// doesn't answer.
func TestPlexPingOffline(t *testing.T) {
	f := newFakePlex(t, 2)
	f.plexTVDown = true
	srv := plexEntry()
	srv.URL, srv.Password = f.URL, "srv"
	if err := New(srv).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestPlexChanged asks for the changed and rated tracks with Plex's raw
// filters, and for the favorites.
func TestPlexChanged(t *testing.T) {
	f := newFakePlex(t, 6)
	f.rated[2] = true
	ch, err := New(plexEntry()).(Syncer).Changed(context.Background(), mustTime(t))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Total != 6 || !ch.Favorites["2_102_1600"] || len(ch.Favorites) != 1 {
		t.Errorf("total %d, favorites %v", ch.Total, ch.Favorites)
	}
	var filters []string
	for _, r := range f.reqs {
		if i := strings.Index(r.URL.RawQuery, ">>="); i > 0 {
			filters = append(filters, r.URL.RawQuery[strings.LastIndex(r.URL.RawQuery[:i], "&")+1:])
		}
	}
	if fmt.Sprint(filters) != "[updatedAt>>=1699999999 lastRatedAt>>=1699999999 userRating>>=9]" {
		t.Errorf("filters = %v", filters)
	}
}

// TestPlexPlaylists keeps only this library's songs and skips the lists
// left empty.
func TestPlexPlaylists(t *testing.T) {
	newFakePlex(t, 3)
	lists, err := New(plexEntry()).Playlists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 || lists[0].Name != "Mixed" || fmt.Sprint(lists[0].Songs) != "[1_101_1600]" {
		t.Errorf("lists = %+v", lists)
	}
}

// TestPlexUserdata sends ratings as 0–10 (a favorite is 10, unrating -1)
// and plays to /:/scrobble, by ratingKey.
func TestPlexUserdata(t *testing.T) {
	f := newFakePlex(t, 1)
	p := New(plexEntry())
	ctx := context.Background()
	for _, err := range []error{p.SetRating(ctx, "4_104_1600", 3), p.SetFavorite(ctx, "4_104_1600", true),
		p.SetFavorite(ctx, "4_104_1600", false), p.Scrobble(ctx, "4_104_1600", mustTime(t))} {
		if err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, r := range f.reqs {
		if strings.HasPrefix(r.URL.Path, "/:/") {
			q := r.URL.Query()
			got = append(got, r.Method+" "+r.URL.Path+" "+q.Get("key")+" "+q.Get("rating"))
		}
	}
	want := "[PUT /:/rate 4 6 PUT /:/rate 4 10 PUT /:/rate 4 -1 GET /:/scrobble 4 ]"
	if fmt.Sprint(got) != want {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// TestPlexURLs builds the file (the default), transcode and cover
// addresses from the id.
func TestPlexURLs(t *testing.T) {
	srv := config.Server{Type: "plex", URL: "http://h:32400/", Password: "tok", Section: "3"}
	p := New(srv)
	if u := p.DownloadURL("4_104_1600"); u != "http://h:32400/library/parts/104/1600/file?download=1&X-Plex-Token=tok" {
		t.Errorf("download = %s", u)
	}
	if u := p.StreamURL("4_104_1600"); !strings.Contains(u, "/library/parts/104/") {
		t.Errorf("default stream = %s, want the original file (like Plexamp)", u)
	}
	srv.Quality = "192"
	u, _ := url.Parse(New(srv).StreamURL("4_104_1600"))
	if q := u.Query(); u.Path != "/audio/:/transcode/universal/start" || q.Get("path") != "/library/metadata/4" || q.Get("musicBitrate") != "192" || q.Get("X-Plex-Platform") != "Generic" {
		t.Errorf("stream = %s", u)
	}
	c, _ := url.Parse(p.CoverURL("1000", 300))
	if c.Path != "/photo/:/transcode" || c.Query().Get("url") != "/library/metadata/1000/thumb" {
		t.Errorf("cover = %s", c)
	}
}

// TestPlexSignIn: the PIN gives the token once entered, and a two-factor
// account asks for the code (ErrPlex2FA) before signing in.
func TestPlexSignIn(t *testing.T) {
	newFakePlex(t, 0)
	ctx := context.Background()
	pin, err := NewPlexPin(ctx)
	if err != nil || pin.Code != "K7Q2" {
		t.Fatalf("pin %+v, %v", pin, err)
	}
	if tok, err := pin.Token(ctx); tok != "acct" || err != nil {
		t.Errorf("pin token %q, %v", tok, err)
	}
	if _, err := PlexSignIn(ctx, "me", "pw", ""); !errors.Is(err, ErrPlex2FA) {
		t.Errorf("without code: %v, want ErrPlex2FA", err)
	}
	if tok, err := PlexSignIn(ctx, "me", "pw", "123456"); tok != "acct" || err != nil {
		t.Errorf("with code: %q, %v", tok, err)
	}
	servers, err := PlexServers(ctx, "acct")
	if err != nil || len(servers) != 1 || servers[0].Machine != "m1" {
		t.Errorf("servers %+v, %v", servers, err)
	}
}

// mustTime is a fixed moment for the tests.
func mustTime(t *testing.T) time.Time { return time.Unix(1_700_000_000, 0) }
