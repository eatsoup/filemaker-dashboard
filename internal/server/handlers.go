package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"filemaker-dashboard/internal/auth"
	"filemaker-dashboard/internal/store"
)

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if u := auth.FromContext(r); u != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.renderPage(w, r, "login.html", map[string]any{
		"Title": "Sign in",
		"Next":  r.URL.Query().Get("next"),
	})
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	u := r.PostFormValue("username")
	p := r.PostFormValue("password")
	tok, _, err := s.Auth.Login(u, p)
	if err != nil {
		s.renderPage(w, r, "login.html", map[string]any{
			"Title": "Sign in",
			"Next":  r.PostFormValue("next"),
			"Error": "Invalid username or password.",
		})
		return
	}
	auth.SetSessionCookie(w, r, tok, s.SessionTTL)
	next := r.PostFormValue("next")
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		_ = s.Auth.Logout(c.Value)
	}
	auth.ClearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) getDashboard(w http.ResponseWriter, r *http.Request) {
	accs, _ := s.Store.DistinctAccounts()
	dbs, _ := s.Store.DistinctDatabases()
	now := time.Now()
	from := now.AddDate(0, 0, -30).Format("2006-01-02")
	to := now.Format("2006-01-02")

	lastSync := ""
	if st, err := s.Store.GetIngestState(); err == nil && !st.LastRun.IsZero() {
		lastSync = st.LastRun.Local().Format("2006-01-02 15:04:05")
	}

	firstRecord, lastRecord := "", ""
	if first, last, err := s.Store.DataRange(); err == nil {
		if !first.IsZero() {
			firstRecord = first.Local().Format("2006-01-02 15:04:05")
		}
		if !last.IsZero() {
			lastRecord = last.Local().Format("2006-01-02 15:04:05")
		}
	}

	s.renderPage(w, r, "dashboard.html", map[string]any{
		"Title":       "Dashboard",
		"Active":      "dashboard",
		"Accounts":    accs,
		"Databases":   dbs,
		"From":        from,
		"To":          to,
		"LastSync":    lastSync,
		"FirstRecord": firstRecord,
		"LastRecord":  lastRecord,
		"Defaults":    s.Defaults,
	})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "account.html", map[string]any{
		"Title":  "Account",
		"Active": "account",
	})
}

func (s *Server) postPassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	me := auth.FromContext(r)
	cur := r.PostFormValue("current")
	newPw := r.PostFormValue("new")
	confirm := r.PostFormValue("confirm")

	flash, kind := "", "ok"
	switch {
	case !auth.CheckPassword(me.PasswordHash, cur):
		flash, kind = "Current password is wrong.", "error"
	case len(newPw) < 6:
		flash, kind = "New password must be at least 6 characters.", "error"
	case newPw != confirm:
		flash, kind = "New passwords do not match.", "error"
	default:
		hash, err := auth.HashPassword(newPw)
		if err != nil {
			flash, kind = "Could not hash password.", "error"
			break
		}
		if err := s.Store.UpdatePassword(me.ID, hash); err != nil {
			flash, kind = "Could not update password.", "error"
			break
		}
		flash, kind = "Password updated.", "ok"
	}

	s.renderPage(w, r, "account.html", map[string]any{
		"Title":     "Account",
		"Active":    "account",
		"Flash":     flash,
		"FlashKind": kind,
	})
}

func (s *Server) getUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.renderPage(w, r, "users.html", map[string]any{
		"Title":  "Users",
		"Active": "users",
		"Users":  users,
		"Me":     auth.FromContext(r),
	})
}

func (s *Server) postUsers(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	isAdmin := r.PostFormValue("is_admin") == "1"

	flash, kind := "", "ok"
	if username == "" || len(password) < 6 {
		flash, kind = "Username and a 6+ character password are required.", "error"
	} else if existing, _ := s.Store.UserByUsername(username); existing != nil {
		flash, kind = "Username already exists.", "error"
	} else {
		hash, err := auth.HashPassword(password)
		if err != nil {
			flash, kind = "Could not hash password.", "error"
		} else if _, err := s.Store.CreateUser(username, hash, isAdmin); err != nil {
			flash, kind = "Could not create user: "+err.Error(), "error"
		} else {
			flash, kind = "User created.", "ok"
		}
	}

	users, _ := s.Store.ListUsers()
	s.renderPage(w, r, "users.html", map[string]any{
		"Title":     "Users",
		"Active":    "users",
		"Users":     users,
		"Me":        auth.FromContext(r),
		"Flash":     flash,
		"FlashKind": kind,
	})
}

func (s *Server) postDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	me := auth.FromContext(r)
	if id == me.ID {
		http.Error(w, "cannot delete yourself", 400)
		return
	}
	if err := s.Store.DeleteUser(id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/users", http.StatusFound)
}

// --- API --------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) buildFilter(r *http.Request) (store.UsageFilter, error) {
	q := r.URL.Query()
	from, to, err := parseDateRange(q.Get("from"), q.Get("to"), 30)
	if err != nil {
		return store.UsageFilter{}, err
	}
	f := store.UsageFilter{
		From:             from,
		To:               to,
		MinDuration:      atoiOr(q.Get("min_duration"), 0),
		MinUsers:         atoiOr(q.Get("min_users"), 0),
		ExcludeUsers:     q["exclude_users"],
		ExcludeDatabases: q["exclude_databases"],
		GroupBy:          q.Get("group_by"),
		Bucket:           pickBucket(from, to),
	}
	return f, nil
}

// pickBucket chooses a bucket size from the requested date range.
//
//	span ≤ 1 day  → 2-hour buckets
//	span ≤ 14 day → daily
//	span < 8 wk   → weekly
//	otherwise     → monthly
func pickBucket(from, to time.Time) string {
	days := int(to.Sub(from).Hours() / 24)
	switch {
	case days <= 1:
		return "2h"
	case days <= 14:
		return "day"
	case days < 56:
		return "week"
	default:
		return "month"
	}
}

func (s *Server) apiUsage(w http.ResponseWriter, r *http.Request) {
	f, err := s.buildFilter(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	pts, err := s.Store.UsageByDay(f)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if pts == nil {
		pts = []store.UsagePoint{}
	}
	writeJSON(w, 200, map[string]any{"points": pts, "bucket": f.Bucket})
}

func (s *Server) apiSummary(w http.ResponseWriter, r *http.Request) {
	f, err := s.buildFilter(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	st, err := s.Store.Summary(f)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) apiFilters(w http.ResponseWriter, r *http.Request) {
	accs, err := s.Store.DistinctAccounts()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	dbs, err := s.Store.DistinctDatabases()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	sort.Strings(accs)
	sort.Strings(dbs)
	writeJSON(w, 200, map[string]any{"users": accs, "databases": dbs})
}
