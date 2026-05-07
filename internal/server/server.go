package server

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"filemaker-dashboard/internal/auth"
	"filemaker-dashboard/internal/config"
	"filemaker-dashboard/internal/store"
)

type Server struct {
	Store      *store.Store
	Auth       *auth.Manager
	pages      map[string]*template.Template // one fully-parsed template per page
	StaticFS   fs.FS
	SessionTTL time.Duration
	Defaults   config.Defaults
	Logger     *slog.Logger
}

var funcMap = template.FuncMap{
	"hours": func(secs int64) string {
		return fmt.Sprintf("%.2f", float64(secs)/3600)
	},
	"contains": func(slice []string, item string) bool {
		for _, s := range slice {
			if s == item {
				return true
			}
		}
		return false
	},
}

// New builds a Server. Templates and static assets must be supplied as embedded
// filesystems by the caller (see main.go).
func New(s *store.Store, a *auth.Manager, ttl time.Duration, defaults config.Defaults, templatesFS, staticFS fs.FS, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}

	baseSrc, err := fs.ReadFile(templatesFS, "_base.html")
	if err != nil {
		return nil, fmt.Errorf("read _base.html: %w", err)
	}

	pages := map[string]*template.Template{}
	entries, err := fs.ReadDir(templatesFS, ".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 5 || name[len(name)-5:] != ".html" || name == "_base.html" {
			continue
		}
		body, err := fs.ReadFile(templatesFS, name)
		if err != nil {
			return nil, err
		}
		t := template.New(name).Funcs(funcMap)
		if _, err := t.Parse(string(baseSrc)); err != nil {
			return nil, fmt.Errorf("parse base for %s: %w", name, err)
		}
		if _, err := t.Parse(string(body)); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		pages[name] = t
	}

	return &Server{
		Store: s, Auth: a, pages: pages,
		StaticFS: staticFS, SessionTTL: ttl, Defaults: defaults, Logger: logger,
	}, nil
}

func (s *Server) Handler(staticPrefix embed.FS) http.Handler {
	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("GET /login", s.getLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", s.postLogout)

	// Static files (no auth)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.StaticFS))))

	// Authenticated UI
	authed := func(h http.HandlerFunc) http.Handler {
		return auth.RequireUser(http.HandlerFunc(h))
	}
	admin := func(h http.HandlerFunc) http.Handler {
		return auth.RequireUser(auth.RequireAdmin(http.HandlerFunc(h)))
	}

	mux.Handle("GET /{$}", authed(s.getDashboard))
	mux.Handle("GET /report", authed(s.getReport))
	mux.Handle("GET /account", authed(s.getAccount))
	mux.Handle("POST /account/password", authed(s.postPassword))

	mux.Handle("GET /users", admin(s.getUsers))
	mux.Handle("POST /users", admin(s.postUsers))
	mux.Handle("POST /users/delete", admin(s.postDeleteUser))

	mux.Handle("GET /api/usage", authed(s.apiUsage))
	mux.Handle("GET /api/summary", authed(s.apiSummary))
	mux.Handle("GET /api/filters", authed(s.apiFilters))

	// Wrap whole tree with LoadUser so handlers can read context.
	return s.Auth.LoadUser(mux)
}

// renderPage executes the base template wired to the named page's content.
func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, pageName string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["User"] = auth.FromContext(r)
	if _, ok := data["Title"]; !ok {
		data["Title"] = "Dashboard"
	}
	t, ok := s.pages[pageName]
	if !ok {
		http.Error(w, fmt.Sprintf("template %q not found", pageName), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		s.Logger.Error("render", "name", pageName, "err", err)
	}
}

// parseDateRange returns from (inclusive) and to (exclusive) UTC timestamps
// based on the user's local-date inputs.
func parseDateRange(from, to string, defDays int) (time.Time, time.Time, error) {
	loc := time.Local
	now := time.Now().In(loc)
	defTo := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	defFrom := defTo.AddDate(0, 0, -defDays)

	t1 := defFrom
	t2 := defTo
	if from != "" {
		x, err := time.ParseInLocation("2006-01-02", from, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("from: %w", err)
		}
		t1 = x
	}
	if to != "" {
		x, err := time.ParseInLocation("2006-01-02", to, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("to: %w", err)
		}
		t2 = x.AddDate(0, 0, 1) // make end-of-day inclusive
	}
	return t1, t2, nil
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
