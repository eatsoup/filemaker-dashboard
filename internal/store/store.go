package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialise writes; SQLite + WAL is fine with this
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  start_time INTEGER NOT NULL,
  end_time INTEGER,
  duration_seconds INTEGER,
  client_name TEXT NOT NULL,
  client_id TEXT NOT NULL,
  account_name TEXT NOT NULL,
  database_name TEXT NOT NULL,
  host TEXT,
  ip TEXT,
  server TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_start ON sessions(start_time);
CREATE INDEX IF NOT EXISTS idx_sessions_end ON sessions(end_time);
CREATE INDEX IF NOT EXISTS idx_sessions_account ON sessions(account_name);
CREATE INDEX IF NOT EXISTS idx_sessions_database ON sessions(database_name);
CREATE INDEX IF NOT EXISTS idx_sessions_open ON sessions(client_id, database_name, account_name) WHERE end_time IS NULL;

CREATE TABLE IF NOT EXISTS ingest_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  last_offset INTEGER NOT NULL DEFAULT 0,
  last_size INTEGER NOT NULL DEFAULT 0,
  last_run INTEGER,
  last_timestamp INTEGER NOT NULL DEFAULT 0,
  last_line_hash TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO ingest_state (id, last_offset, last_size) VALUES (1, 0, 0);

CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT UNIQUE NOT NULL COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS web_sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_web_sessions_expires ON web_sessions(expires_at);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Older databases predate the (timestamp, line-hash) ingest cursor;
	// add the columns if they're missing.
	if err := s.ensureColumn("ingest_state", "last_timestamp", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("ingest_state", "last_line_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Sessions are now keyed naturally on (start_time, client_id, account, database) so
	// re-processing a log line is a no-op. Drop accidental duplicates left behind by the
	// old offset-based ingester before adding the unique index.
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id NOT IN (
        SELECT MIN(id) FROM sessions
        GROUP BY start_time, client_id, account_name, database_name
    )`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_natural
        ON sessions(start_time, client_id, account_name, database_name)`); err != nil {
		return err
	}
	// Seed the timestamp cursor from existing data so the first run after upgrade
	// doesn't try to re-walk months of log lines.
	if _, err := s.db.Exec(`UPDATE ingest_state SET last_timestamp = COALESCE((
        SELECT MAX(t) FROM (
            SELECT MAX(start_time) AS t FROM sessions
            UNION ALL
            SELECT MAX(end_time) AS t FROM sessions WHERE end_time IS NOT NULL
        )
    ), 0) WHERE last_timestamp = 0`); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(table, col, ddl string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, ddl))
	return err
}

// --- ingest state -----------------------------------------------------------

// IngestState carries the cursor that lets the ingester resume across runs.
// LastTimestamp is the unix-second timestamp of the most recent log line we
// processed; LastLineHash is its hex SHA-256, used to disambiguate multiple
// lines that share that second.
type IngestState struct {
	LastTimestamp int64
	LastLineHash  string
	LastRun       time.Time
}

func (s *Store) GetIngestState() (IngestState, error) {
	var st IngestState
	var lastRun sql.NullInt64
	err := s.db.QueryRow(`SELECT last_timestamp, last_line_hash, last_run FROM ingest_state WHERE id = 1`).
		Scan(&st.LastTimestamp, &st.LastLineHash, &lastRun)
	if err != nil {
		return st, err
	}
	if lastRun.Valid {
		st.LastRun = time.Unix(lastRun.Int64, 0)
	}
	return st, nil
}

func (s *Store) SetIngestState(st IngestState) error {
	_, err := s.db.Exec(
		`UPDATE ingest_state SET last_timestamp = ?, last_line_hash = ?, last_run = ? WHERE id = 1`,
		st.LastTimestamp, st.LastLineHash, st.LastRun.Unix(),
	)
	return err
}

// --- sessions ---------------------------------------------------------------

type Session struct {
	StartTime  time.Time
	EndTime    *time.Time
	ClientName string
	ClientID   string
	Account    string
	Database   string
	Host       string
	IP         string
	Server     string
}

// IngestBatch is a short-lived transaction wrapper used by the log ingester
// to apply many open/close events atomically (and far faster than autocommit).
type IngestBatch struct {
	tx *sql.Tx
}

func (s *Store) BeginIngest() (*IngestBatch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &IngestBatch{tx: tx}, nil
}

func (b *IngestBatch) Commit() error   { return b.tx.Commit() }
func (b *IngestBatch) Rollback() error { return b.tx.Rollback() }

// OpenSession inserts a new in-flight session row (end_time NULL).
// Returns true if a new row was actually inserted; INSERT OR IGNORE makes
// re-processing the same open line idempotent against the natural-key
// unique index.
func (b *IngestBatch) OpenSession(sess Session) (bool, error) {
	res, err := b.tx.Exec(
		`INSERT OR IGNORE INTO sessions (start_time, client_name, client_id, account_name, database_name, host, ip, server)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.StartTime.Unix(), sess.ClientName, sess.ClientID, sess.Account, sess.Database, sess.Host, sess.IP, sess.Server,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CloseSession finds the oldest in-flight session matching (client_id, db, account)
// that started no later than the close time, and sets its end_time/duration.
// The start_time bound prevents a stale close from being applied to a session
// that opened after it (e.g. when --import replays an older rotated log).
// Returns true if a row was updated.
func (b *IngestBatch) CloseSession(clientID, database, account string, end time.Time) (bool, error) {
	row := b.tx.QueryRow(
		`SELECT id, start_time FROM sessions
         WHERE client_id = ? AND database_name = ? AND account_name = ?
           AND end_time IS NULL AND start_time <= ?
         ORDER BY start_time ASC LIMIT 1`,
		clientID, database, account, end.Unix(),
	)
	var id, startUnix int64
	if err := row.Scan(&id, &startUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	dur := end.Unix() - startUnix
	if dur < 0 {
		dur = 0
	}
	_, err := b.tx.Exec(
		`UPDATE sessions SET end_time = ?, duration_seconds = ? WHERE id = ?`,
		end.Unix(), dur, id,
	)
	if err != nil {
		return false, err
	}
	return true, nil
}

// SetIngestState writes ingest state inside the batch's transaction so the
// cursor advances atomically with the inserts.
func (b *IngestBatch) SetIngestState(st IngestState) error {
	_, err := b.tx.Exec(
		`UPDATE ingest_state SET last_timestamp = ?, last_line_hash = ?, last_run = ? WHERE id = 1`,
		st.LastTimestamp, st.LastLineHash, st.LastRun.Unix(),
	)
	return err
}

// --- users ------------------------------------------------------------------

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time
}

func (s *Store) CreateUser(username, passwordHash string, isAdmin bool) (int64, error) {
	now := time.Now().Unix()
	admin := 0
	if isAdmin {
		admin = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, is_admin, created_at) VALUES (?, ?, ?, ?)`,
		username, passwordHash, admin, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UserByUsername(username string) (*User, error) {
	u := &User{}
	var admin int
	var created int64
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE username = ? COLLATE NOCASE`,
		username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = admin != 0
	u.CreatedAt = time.Unix(created, 0)
	return u, nil
}

func (s *Store) UserByID(id int64) (*User, error) {
	u := &User{}
	var admin int
	var created int64
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE id = ?`,
		id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = admin != 0
	u.CreatedAt = time.Unix(created, 0)
	return u, nil
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(
		`SELECT id, username, password_hash, is_admin, created_at FROM users ORDER BY username`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u := User{}
		var admin int
		var created int64
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &created); err != nil {
			return nil, err
		}
		u.IsAdmin = admin != 0
		u.CreatedAt = time.Unix(created, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdatePassword(userID int64, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// --- web sessions -----------------------------------------------------------

func (s *Store) CreateWebSession(token string, userID int64, expires time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO web_sessions (token, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		token, userID, time.Now().Unix(), expires.Unix(),
	)
	return err
}

func (s *Store) UserBySessionToken(token string) (*User, error) {
	row := s.db.QueryRow(
		`SELECT u.id, u.username, u.password_hash, u.is_admin, u.created_at
         FROM web_sessions w JOIN users u ON u.id = w.user_id
         WHERE w.token = ? AND w.expires_at > ?`,
		token, time.Now().Unix(),
	)
	u := &User{}
	var admin int
	var created int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &admin, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = admin != 0
	u.CreatedAt = time.Unix(created, 0)
	return u, nil
}

func (s *Store) DeleteWebSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE token = ?`, token)
	return err
}

func (s *Store) PurgeExpiredWebSessions() error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE expires_at <= ?`, time.Now().Unix())
	return err
}

// --- queries for the dashboard ---------------------------------------------

type UsageFilter struct {
	From             time.Time
	To               time.Time
	MinDuration      int // seconds
	MinUsers         int // exclude databases with fewer than this many distinct users in the window; <=1 disables
	ExcludeUsers     []string
	ExcludeDatabases []string
	GroupBy          string // "user" or "database"
	Bucket           string // "2h", "day", "week", "month" — empty defaults to "day"
}

// minUsersClause returns a SQL fragment beginning with " AND database_name IN (...)"
// that restricts to databases meeting the minUsers threshold within the same
// window and other filters. Returns ("", nil) when no restriction is needed.
func minUsersClause(from, to time.Time, minDuration, minUsers int, excludeUsers, excludeDBs []string) (string, []any) {
	if minUsers <= 1 {
		return "", nil
	}
	sub := `(SELECT database_name FROM sessions
             WHERE end_time IS NOT NULL
               AND start_time >= ? AND start_time < ?
               AND COALESCE(duration_seconds, 0) >= ?`
	args := []any{from.Unix(), to.Unix(), minDuration}
	if len(excludeUsers) > 0 {
		sub += " AND account_name NOT IN (" + placeholders(len(excludeUsers)) + ")"
		for _, u := range excludeUsers {
			args = append(args, u)
		}
	}
	if len(excludeDBs) > 0 {
		sub += " AND database_name NOT IN (" + placeholders(len(excludeDBs)) + ")"
		for _, d := range excludeDBs {
			args = append(args, d)
		}
	}
	sub += " GROUP BY database_name HAVING COUNT(DISTINCT account_name) >= ?)"
	args = append(args, minUsers)
	return " AND database_name IN " + sub, args
}

// monthlyMinUsersClause is like minUsersClause but evaluated per (database, month):
// a session is kept only if its (database_name, month) bucket itself meets the
// threshold. Used by the report/billing grids whose cells are monthly.
func monthlyMinUsersClause(from, to time.Time, minDuration, minUsers int, excludeUsers, excludeDBs []string) (string, []any) {
	if minUsers <= 1 {
		return "", nil
	}
	const monthExpr = `strftime('%Y-%m', start_time, 'unixepoch', 'localtime')`
	sub := `(SELECT database_name, ` + monthExpr + ` AS m FROM sessions
             WHERE end_time IS NOT NULL
               AND start_time >= ? AND start_time < ?
               AND COALESCE(duration_seconds, 0) >= ?`
	args := []any{from.Unix(), to.Unix(), minDuration}
	if len(excludeUsers) > 0 {
		sub += " AND account_name NOT IN (" + placeholders(len(excludeUsers)) + ")"
		for _, u := range excludeUsers {
			args = append(args, u)
		}
	}
	if len(excludeDBs) > 0 {
		sub += " AND database_name NOT IN (" + placeholders(len(excludeDBs)) + ")"
		for _, d := range excludeDBs {
			args = append(args, d)
		}
	}
	sub += " GROUP BY database_name, m HAVING COUNT(DISTINCT account_name) >= ?)"
	args = append(args, minUsers)
	return " AND (database_name, " + monthExpr + ") IN " + sub, args
}

type UsagePoint struct {
	Day      string  `json:"day"`     // bucket label (format depends on bucket)
	Group    string  `json:"group"`   // user or database
	Seconds  int64   `json:"seconds"` // total session-seconds in that bucket/group
}

// bucketExpr returns the SQL expression for the bucket label.
func bucketExpr(bucket string) string {
	switch bucket {
	case "2h":
		return `strftime('%Y-%m-%d ', start_time, 'unixepoch', 'localtime') || ` +
			`printf('%02d:00', (cast(strftime('%H', start_time, 'unixepoch', 'localtime') as integer) / 2) * 2)`
	case "week":
		return `date(start_time, 'unixepoch', 'localtime', '-' || ` +
			`((cast(strftime('%w', start_time, 'unixepoch', 'localtime') as integer) + 6) % 7) || ' days')`
	case "month":
		return `strftime('%Y-%m', start_time, 'unixepoch', 'localtime')`
	default:
		return `strftime('%Y-%m-%d', start_time, 'unixepoch', 'localtime')`
	}
}

func (s *Store) UsageByDay(f UsageFilter) ([]UsagePoint, error) {
	var groupCol string
	switch f.GroupBy {
	case "database":
		groupCol = "database_name"
	default:
		groupCol = "account_name"
	}

	q := `SELECT
            ` + bucketExpr(f.Bucket) + ` AS day,
            ` + groupCol + ` AS grp,
            SUM(COALESCE(duration_seconds, 0)) AS secs
          FROM sessions
          WHERE end_time IS NOT NULL
            AND start_time >= ? AND start_time < ?
            AND COALESCE(duration_seconds, 0) >= ?`
	args := []any{f.From.Unix(), f.To.Unix(), f.MinDuration}

	if len(f.ExcludeUsers) > 0 {
		q += " AND account_name NOT IN (" + placeholders(len(f.ExcludeUsers)) + ")"
		for _, u := range f.ExcludeUsers {
			args = append(args, u)
		}
	}
	if len(f.ExcludeDatabases) > 0 {
		q += " AND database_name NOT IN (" + placeholders(len(f.ExcludeDatabases)) + ")"
		for _, d := range f.ExcludeDatabases {
			args = append(args, d)
		}
	}
	if frag, fargs := minUsersClause(f.From, f.To, f.MinDuration, f.MinUsers, f.ExcludeUsers, f.ExcludeDatabases); frag != "" {
		q += frag
		args = append(args, fargs...)
	}
	q += " GROUP BY day, grp ORDER BY day, grp"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsagePoint
	for rows.Next() {
		var p UsagePoint
		if err := rows.Scan(&p.Day, &p.Group, &p.Seconds); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}

// DataRange returns the earliest start_time and the latest of (start_time,
// end_time) across the sessions table. Either return value is the zero time
// if the table is empty.
func (s *Store) DataRange() (time.Time, time.Time, error) {
	var minStart, maxStart sql.NullInt64
	var maxEnd sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MIN(start_time), MAX(start_time), MAX(end_time) FROM sessions`,
	).Scan(&minStart, &maxStart, &maxEnd)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	var first, last time.Time
	if minStart.Valid {
		first = time.Unix(minStart.Int64, 0)
	}
	latest := int64(0)
	if maxStart.Valid && maxStart.Int64 > latest {
		latest = maxStart.Int64
	}
	if maxEnd.Valid && maxEnd.Int64 > latest {
		latest = maxEnd.Int64
	}
	if latest > 0 {
		last = time.Unix(latest, 0)
	}
	return first, last, nil
}

func (s *Store) DistinctAccounts() ([]string, error) {
	return s.distinct("account_name")
}
func (s *Store) DistinctDatabases() ([]string, error) {
	return s.distinct("database_name")
}
func (s *Store) distinct(col string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT ` + col + ` FROM sessions ORDER BY ` + col)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type ReportRow struct {
	Month        string `json:"month"` // YYYY-MM
	Key          string `json:"key"`   // user or database, depending on groupBy
	TotalSeconds int64  `json:"total_seconds"`
	SessionCount int64  `json:"session_count"`
	DistinctDBs  int64  `json:"distinct_dbs"`
}

// MonthlyReport aggregates session totals by month and groupBy ("user" or "database").
func (s *Store) MonthlyReport(from, to time.Time, minDuration, minUsers int, groupBy string, excludeUsers, excludeDBs []string) ([]ReportRow, error) {
	groupCol := "account_name"
	if groupBy == "database" {
		groupCol = "database_name"
	}
	q := `SELECT
            strftime('%Y-%m', start_time, 'unixepoch', 'localtime') AS month,
            ` + groupCol + ` AS grp,
            SUM(COALESCE(duration_seconds, 0)),
            COUNT(*),
            COUNT(DISTINCT database_name)
          FROM sessions
          WHERE end_time IS NOT NULL
            AND start_time >= ? AND start_time < ?
            AND COALESCE(duration_seconds, 0) >= ?`
	args := []any{from.Unix(), to.Unix(), minDuration}
	if len(excludeUsers) > 0 {
		q += " AND account_name NOT IN (" + placeholders(len(excludeUsers)) + ")"
		for _, u := range excludeUsers {
			args = append(args, u)
		}
	}
	if len(excludeDBs) > 0 {
		q += " AND database_name NOT IN (" + placeholders(len(excludeDBs)) + ")"
		for _, d := range excludeDBs {
			args = append(args, d)
		}
	}
	if frag, fargs := monthlyMinUsersClause(from, to, minDuration, minUsers, excludeUsers, excludeDBs); frag != "" {
		q += frag
		args = append(args, fargs...)
	}
	q += ` GROUP BY month, grp ORDER BY month, grp`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReportRow
	for rows.Next() {
		var r ReportRow
		if err := rows.Scan(&r.Month, &r.Key, &r.TotalSeconds, &r.SessionCount, &r.DistinctDBs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BillingMonthRow is one (month, database) row used to build the billing matrix.
type BillingMonthRow struct {
	Month        string `json:"month"`
	Database     string `json:"database"`
	TotalSeconds int64  `json:"total_seconds"`
	UniqueUsers  int64  `json:"unique_users"`
}

// BillingByMonth returns one row per (month, database) with usage in the
// requested window. Rows where total seconds < minDuration are excluded so that
// trivial connections don't get a customer billed for the database.
func (s *Store) BillingByMonth(from, to time.Time, minDuration, minUsers int, excludeUsers, excludeDBs []string) ([]BillingMonthRow, error) {
	q := `SELECT
            strftime('%Y-%m', start_time, 'unixepoch', 'localtime') AS month,
            database_name,
            SUM(COALESCE(duration_seconds, 0)) AS secs,
            COUNT(DISTINCT account_name) AS users
          FROM sessions
          WHERE end_time IS NOT NULL
            AND start_time >= ? AND start_time < ?
            AND COALESCE(duration_seconds, 0) >= ?`
	args := []any{from.Unix(), to.Unix(), minDuration}
	if len(excludeUsers) > 0 {
		q += " AND account_name NOT IN (" + placeholders(len(excludeUsers)) + ")"
		for _, u := range excludeUsers {
			args = append(args, u)
		}
	}
	if len(excludeDBs) > 0 {
		q += " AND database_name NOT IN (" + placeholders(len(excludeDBs)) + ")"
		for _, d := range excludeDBs {
			args = append(args, d)
		}
	}
	if frag, fargs := monthlyMinUsersClause(from, to, minDuration, minUsers, excludeUsers, excludeDBs); frag != "" {
		q += frag
		args = append(args, fargs...)
	}
	q += ` GROUP BY month, database_name HAVING secs > 0 ORDER BY month, database_name`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BillingMonthRow
	for rows.Next() {
		var r BillingMonthRow
		if err := rows.Scan(&r.Month, &r.Database, &r.TotalSeconds, &r.UniqueUsers); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SummaryStats returns top-level numbers shown above the chart.
type SummaryStats struct {
	TotalSessions int64 `json:"total_sessions"`
	TotalSeconds  int64 `json:"total_seconds"`
	UniqueUsers   int64 `json:"unique_users"`
	UniqueDBs     int64 `json:"unique_dbs"`
}

func (s *Store) Summary(f UsageFilter) (SummaryStats, error) {
	q := `SELECT
            COUNT(*),
            COALESCE(SUM(duration_seconds), 0),
            COUNT(DISTINCT account_name),
            COUNT(DISTINCT database_name)
          FROM sessions
          WHERE end_time IS NOT NULL
            AND start_time >= ? AND start_time < ?
            AND COALESCE(duration_seconds, 0) >= ?`
	args := []any{f.From.Unix(), f.To.Unix(), f.MinDuration}
	if len(f.ExcludeUsers) > 0 {
		q += " AND account_name NOT IN (" + placeholders(len(f.ExcludeUsers)) + ")"
		for _, u := range f.ExcludeUsers {
			args = append(args, u)
		}
	}
	if len(f.ExcludeDatabases) > 0 {
		q += " AND database_name NOT IN (" + placeholders(len(f.ExcludeDatabases)) + ")"
		for _, d := range f.ExcludeDatabases {
			args = append(args, d)
		}
	}
	if frag, fargs := minUsersClause(f.From, f.To, f.MinDuration, f.MinUsers, f.ExcludeUsers, f.ExcludeDatabases); frag != "" {
		q += frag
		args = append(args, fargs...)
	}
	var st SummaryStats
	err := s.db.QueryRow(q, args...).Scan(&st.TotalSessions, &st.TotalSeconds, &st.UniqueUsers, &st.UniqueDBs)
	if err != nil {
		return st, fmt.Errorf("summary: %w", err)
	}
	return st, nil
}
