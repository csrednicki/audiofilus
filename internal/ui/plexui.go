package ui

import (
	"cmp"
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/hajimehoshi/ebiten/v2"

	"github.com/juanvillacortac/audiofilus/internal/config"
	"github.com/juanvillacortac/audiofilus/internal/remote"
)

// Adding a Plex server: sign in on plex.tv (a code for plex.tv/link, or
// login and password), then pick the server and the music library. Each
// library becomes its own server entry. old is the entry being fixed (its
// name, quality and cache size are kept), or empty.

// later runs f on the interface's thread; it's for goroutines.
func (a *App) later(f func()) {
	a.acctCh <- f
	ebiten.ScheduleFrame()
}

// plexFlow asks how to sign in.
func (a *App) plexFlow(old config.Server) {
	a.menu = &menu{title: "Plex", subtitle: T("plex_how"), items: []menuItem{
		{label: T("plex_pin"), icon: "globe", do: func() { a.plexPin(old) }},
		{label: T("plex_login"), icon: "user", do: func() { a.plexLogin(old) }},
	}}
}

// plexPin shows a code to enter on plex.tv/link and asks plex.tv every 3
// seconds whether it was entered, until it is or the screen is closed.
func (a *App) plexPin(old config.Server) {
	a.flash(T("srv_testing", "plex.tv"))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		pin, err := remote.NewPlexPin(ctx)
		if err != nil {
			a.later(func() { a.flash(T("acct_error", err)) })
			return
		}
		m := &menu{title: "Plex", subtitle: T("plex_pin_hint"), items: []menuItem{
			{label: pin.Code, icon: "globe", value: func() string { return "plex.tv/link" }, do: func() {}},
		}}
		a.later(func() { a.menu = m })
		var gone atomic.Bool
		for !gone.Load() && ctx.Err() == nil {
			time.Sleep(3 * time.Second)
			a.later(func() {
				if a.menu != m {
					gone.Store(true)
				}
			})
			tok, err := pin.Token(ctx)
			if err != nil || tok == "" {
				continue // not entered yet (or plex.tv didn't answer this time)
			}
			a.later(func() {
				if a.menu == m {
					a.plexServers(old, tok)
				}
			})
			return
		}
	}()
}

// plexLogin asks for the Plex login and password.
func (a *App) plexLogin(old config.Server) {
	a.openRawPrompt(T("plex_user"), "", false, func(login string) {
		a.openRawPrompt(T("srv_pass"), "", true, func(pass string) {
			a.plexSignIn(old, login, pass, "")
		})
	})
}

// plexSignIn signs in on plex.tv, asking for the two-factor code if the
// account has it.
func (a *App) plexSignIn(old config.Server, login, pass, code string) {
	a.flash(T("srv_testing", "plex.tv"))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		tok, err := remote.PlexSignIn(ctx, login, pass, code)
		a.later(func() {
			switch {
			case errors.Is(err, remote.ErrPlex2FA) && code == "":
				a.openRawPrompt(T("plex_2fa"), "", false, func(c string) { a.plexSignIn(old, login, pass, c) })
			case err != nil:
				a.flash(T("acct_error", err))
			default:
				a.plexServers(old, tok)
			}
		})
	}()
}

// plexServers lists the account's servers; with only one there is nothing
// to ask.
func (a *App) plexServers(old config.Server, token string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		servers, err := remote.PlexServers(ctx, token)
		a.later(func() {
			switch {
			case err != nil:
				a.flash(T("acct_error", err))
			case len(servers) == 0:
				a.flash(T("plex_no_servers"))
			case len(servers) == 1:
				a.plexSections(old, token, servers[0])
			default:
				m := &menu{title: "Plex", subtitle: T("plex_server")}
				for _, s := range servers {
					m.items = append(m.items, menuItem{label: s.Name, icon: "server", do: func() { a.plexSections(old, token, s) }})
				}
				a.menu = m
			}
		})
	}()
}

// plexSections connects to the server and lists its music libraries.
func (a *App) plexSections(old config.Server, token string, s remote.PlexServer) {
	a.flash(T("srv_testing", s.Name))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		base, err := s.Connect(ctx)
		var secs []remote.PlexSection
		if err == nil {
			secs, err = remote.PlexSections(ctx, base, cmp.Or(s.Token, token))
		}
		a.later(func() {
			switch {
			case err != nil:
				a.flash(T("acct_error", err))
			case len(secs) == 0:
				a.flash(T("plex_no_music", s.Name))
			case len(secs) == 1:
				a.plexSave(old, token, s, base, secs[0])
			default:
				m := &menu{title: s.Name, subtitle: T("plex_section")}
				for _, sec := range secs {
					m.items = append(m.items, menuItem{label: sec.Title, icon: "disc", do: func() { a.plexSave(old, token, s, base, sec) }})
				}
				a.menu = m
			}
		})
	}()
}

// plexSave asks for the entry's name and tests it like any other server.
func (a *App) plexSave(old config.Server, token string, s remote.PlexServer, base string, sec remote.PlexSection) {
	srv := config.Server{Name: s.Name + " " + sec.Title, Type: "plex", URL: base, Password: token,
		Machine: s.Machine, Section: sec.Key, Quality: old.Quality, CacheMB: old.CacheMB}
	if old.Machine == s.Machine && old.Section == sec.Key {
		srv.Name = old.Name // the same library: keep its name and so its cache
	}
	a.openPrompt(T("srv_name"), srv.Name, func(name string) {
		srv.Name = name
		a.pingServer(srv, true)
	})
	a.prompt.typed = true
}
