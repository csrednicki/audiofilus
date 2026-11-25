package remote

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Lo de cada usuario en el servidor: favoritas, valoraciones y escuchas.
// En Navidrome las escuchas además se reenvían a Last.fm y ListenBrainz si
// el usuario lo tiene configurado allí.

// SetFavorite marca la canción con star o unstar.
func (c *Client) SetFavorite(ctx context.Context, id string, on bool) error {
	method := "unstar"
	if on {
		method = "star"
	}
	return c.get(ctx, method, url.Values{"id": {id}}, nil)
}

// SetRating valora la canción (0 quita la valoración).
func (c *Client) SetRating(ctx context.Context, id string, rating int) error {
	return c.get(ctx, "setRating", url.Values{"id": {id}, "rating": {strconv.Itoa(max(0, min(5, rating)))}}, nil)
}

// Scrobble cuenta una escucha (submission=true: no solo "sonando ahora").
func (c *Client) Scrobble(ctx context.Context, id string, at time.Time) error {
	return c.get(ctx, "scrobble", url.Values{"id": {id}, "time": {strconv.FormatInt(at.UnixMilli(), 10)}, "submission": {"true"}}, nil)
}

// SetFavorite marca o desmarca la canción entre las favoritas del usuario.
func (j *jellyfin) SetFavorite(ctx context.Context, id string, on bool) error {
	if err := j.login(ctx); err != nil {
		return err
	}
	method := http.MethodDelete
	if on {
		method = http.MethodPost
	}
	return j.call(ctx, method, fmt.Sprintf("/Users/%s/FavoriteItems/%s", j.user(), id), nil, nil)
}

// SetRating no hace nada: Jellyfin no valora canciones de 1 a 5.
func (j *jellyfin) SetRating(ctx context.Context, id string, rating int) error { return nil }

// Scrobble marca la canción como escuchada (suma una reproducción).
func (j *jellyfin) Scrobble(ctx context.Context, id string, at time.Time) error {
	if err := j.login(ctx); err != nil {
		return err
	}
	q := url.Values{"datePlayed": {at.UTC().Format("2006-01-02T15:04:05.000Z")}}
	return j.call(ctx, http.MethodPost, fmt.Sprintf("/Users/%s/PlayedItems/%s?%s", j.user(), id, q.Encode()), nil, nil)
}

// SetFavorite: Plex has no favorites, so a favorite is 5 stars (rating 10,
// like Plexamp's "love"). Unmarking it also removes the rating.
func (p *plex) SetFavorite(ctx context.Context, id string, on bool) error {
	r := 0
	if on {
		r = 5
	}
	return p.SetRating(ctx, id, r)
}

// SetRating rates the track 1 to 5 stars (Plex counts 0 to 10); 0 removes
// the rating.
func (p *plex) SetRating(ctx context.Context, id string, rating int) error {
	key, _ := splitPlexID(id)
	r := -1
	if rating > 0 {
		r = 2 * min(5, rating)
	}
	q := url.Values{"key": {key}, "identifier": {plexIdentifier}, "rating": {strconv.Itoa(r)}}
	return p.get(ctx, http.MethodPut, "/:/rate?"+q.Encode(), nil)
}

// Scrobble marks the track as played (adds one to its play count).
func (p *plex) Scrobble(ctx context.Context, id string, at time.Time) error {
	key, _ := splitPlexID(id)
	q := url.Values{"key": {key}, "identifier": {plexIdentifier}}
	return p.get(ctx, http.MethodGet, "/:/scrobble?"+q.Encode(), nil)
}

// user es el id del usuario con la sesión iniciada.
func (j *jellyfin) user() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.userID
}

// SetFavorite, SetRating y Scrobble hacen lo suyo con una canción remota
// (path "subsonic://servidor/id") en su servidor.
func (s *Store) SetFavorite(ctx context.Context, path string, on bool) error {
	c, id, err := s.song(path)
	if err != nil {
		return err
	}
	return c.SetFavorite(ctx, id, on)
}

func (s *Store) SetRating(ctx context.Context, path string, rating int) error {
	c, id, err := s.song(path)
	if err != nil {
		return err
	}
	return c.SetRating(ctx, id, rating)
}

func (s *Store) Scrobble(ctx context.Context, path string, at time.Time) error {
	c, id, err := s.song(path)
	if err != nil {
		return err
	}
	return c.Scrobble(ctx, id, at)
}

// song es el cliente y el id de una canción remota.
func (s *Store) song(path string) (Backend, string, error) {
	server, cover, id, ok := Parse(path)
	if !ok || cover {
		return nil, "", fmt.Errorf("remote: not a song: %s", path)
	}
	c, _, err := s.client(server)
	if err != nil {
		return nil, "", err
	}
	return c, id, nil
}
