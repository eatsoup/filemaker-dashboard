package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"filemaker-dashboard/internal/store"
)

type reportRowView struct {
	Key   string
	Cells []string
	Total string
}

type billingMonthCol struct {
	Month     string   // YYYY-MM
	DBCount   int      // distinct databases active in that month
	TotalHrs  string   // total hours across all DBs that month
	Databases []string // sorted list of databases active that month
}

type billingDBRow struct {
	Database string
	Cells    []string // unique-user count for that month (or "" if inactive)
	Active   int      // count of months this DB was active
	Total    string   // total hours over the period
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	// Default: previous 3 full months including this one.
	defTo := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local).
		AddDate(0, 1, 0).AddDate(0, 0, -1)
	defFrom := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local).
		AddDate(0, -2, 0)

	fromStr := q.Get("from")
	toStr := q.Get("to")
	if fromStr == "" {
		fromStr = defFrom.Format("2006-01-02")
	}
	if toStr == "" {
		toStr = defTo.Format("2006-01-02")
	}
	from, to, err := parseDateRange(fromStr, toStr, 90)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	initial := r.URL.RawQuery == ""
	minDur := atoiOr(q.Get("min_duration"), 0)
	minUsers := atoiOr(q.Get("min_users"), 0)
	groupBy := q.Get("group_by")
	excludeUsers := q["exclude_users"]
	excludeDBs := q["exclude_databases"]
	if initial {
		minDur = s.Defaults.MinDuration
		minUsers = s.Defaults.MinUsers
		groupBy = s.Defaults.GroupBy
		excludeUsers = s.Defaults.ExcludeUsers
		excludeDBs = s.Defaults.ExcludeDatabases
	}
	if groupBy != "database" {
		groupBy = "user"
	}
	header := "User"
	if groupBy == "database" {
		header = "Database"
	}

	rows, err := s.Store.MonthlyReport(from, to, minDur, minUsers, groupBy, excludeUsers, excludeDBs)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Pivot: build month list and per-key row of hours.
	monthSet := map[string]bool{}
	keyMap := map[string]map[string]int64{}
	for _, r := range rows {
		monthSet[r.Month] = true
		m, ok := keyMap[r.Key]
		if !ok {
			m = map[string]int64{}
			keyMap[r.Key] = m
		}
		m[r.Month] += r.TotalSeconds
	}
	months := make([]string, 0, len(monthSet))
	for m := range monthSet {
		months = append(months, m)
	}
	sort.Strings(months)

	keys := make([]string, 0, len(keyMap))
	for k := range keyMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	view := make([]reportRowView, 0, len(keys))
	monthTotals := make([]int64, len(months))
	var grand int64
	for _, k := range keys {
		secsByMonth := keyMap[k]
		cells := make([]string, len(months))
		var rowTotal int64
		for i, m := range months {
			s := secsByMonth[m]
			cells[i] = fmtHours(s)
			rowTotal += s
			monthTotals[i] += s
		}
		grand += rowTotal
		view = append(view, reportRowView{
			Key: k, Cells: cells, Total: fmtHours(rowTotal),
		})
	}
	totalsStr := make([]string, len(monthTotals))
	for i, t := range monthTotals {
		totalsStr[i] = fmtHours(t)
	}

	billing, err := s.Store.BillingByMonth(from, to, minDur, minUsers, excludeUsers, excludeDBs)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	billCols, billRows := buildBillingView(billing)

	billingTotal := 0
	for _, c := range billCols {
		billingTotal += c.DBCount
	}
	periodLabel := periodLabel(fromStr, toStr)

	s.renderPage(w, r, "report.html", map[string]any{
		"Title":            "Monthly report",
		"Active":           "report",
		"From":             fromStr,
		"To":               toStr,
		"MinDuration":      minDur,
		"MinUsers":         minUsers,
		"GroupBy":          groupBy,
		"Header":           header,
		"Months":           months,
		"Rows":             view,
		"Totals":           totalsStr,
		"GrandTotal":       fmtHours(grand),
		"BillingCols":      billCols,
		"BillingRows":      billRows,
		"BillingTotal":     billingTotal,
		"PeriodLabel":      periodLabel,
		"ExcludeUsers":     excludeUsers,
		"ExcludeDatabases": excludeDBs,
	})
}

// periodLabel returns "Qn YYYY" if from/to span exactly one calendar quarter,
// otherwise the literal date range.
func periodLabel(fromStr, toStr string) string {
	from, err1 := time.Parse("2006-01-02", fromStr)
	to, err2 := time.Parse("2006-01-02", toStr)
	if err1 == nil && err2 == nil && from.Year() == to.Year() && from.Day() == 1 {
		startMonth := int(from.Month())
		endMonth := int(to.Month())
		if startMonth%3 == 1 && endMonth == startMonth+2 {
			lastDay := time.Date(from.Year(), time.Month(endMonth+1), 0, 0, 0, 0, 0, time.Local).Day()
			if to.Day() == lastDay {
				q := (startMonth-1)/3 + 1
				return fmt.Sprintf("Q%d %d", q, from.Year())
			}
		}
	}
	return fromStr + " to " + toStr
}

// buildBillingView turns a flat (month, db, secs) result into:
//   - one column per month with the distinct-database count + total hours
//   - one row per database with ✓ marks for the months it was active
func buildBillingView(rows []store.BillingMonthRow) ([]billingMonthCol, []billingDBRow) {
	monthSet := map[string]bool{}
	dbSet := map[string]bool{}
	hours := map[string]map[string]int64{} // month → db → secs
	users := map[string]map[string]int64{} // month → db → distinct users
	for _, r := range rows {
		monthSet[r.Month] = true
		dbSet[r.Database] = true
		if hours[r.Month] == nil {
			hours[r.Month] = map[string]int64{}
			users[r.Month] = map[string]int64{}
		}
		hours[r.Month][r.Database] += r.TotalSeconds
		users[r.Month][r.Database] = r.UniqueUsers
	}
	months := make([]string, 0, len(monthSet))
	for m := range monthSet {
		months = append(months, m)
	}
	sort.Strings(months)
	dbs := make([]string, 0, len(dbSet))
	for d := range dbSet {
		dbs = append(dbs, d)
	}
	sort.Strings(dbs)

	cols := make([]billingMonthCol, len(months))
	for i, m := range months {
		var totalSecs int64
		var active []string
		for db, secs := range hours[m] {
			if secs > 0 {
				active = append(active, db)
				totalSecs += secs
			}
		}
		sort.Strings(active)
		cols[i] = billingMonthCol{
			Month: m, DBCount: len(active), TotalHrs: fmtHours(totalSecs), Databases: active,
		}
	}

	out := make([]billingDBRow, 0, len(dbs))
	for _, db := range dbs {
		cells := make([]string, len(months))
		var rowSecs int64
		var active int
		for i, m := range months {
			s := hours[m][db]
			rowSecs += s
			if s > 0 {
				cells[i] = strconv.FormatInt(users[m][db], 10)
				active++
			} else {
				cells[i] = ""
			}
		}
		out = append(out, billingDBRow{
			Database: db, Cells: cells, Active: active, Total: fmtHours(rowSecs),
		})
	}
	return cols, out
}

func fmtHours(secs int64) string {
	if secs == 0 {
		return "–"
	}
	return fmt.Sprintf("%.2f h", float64(secs)/3600)
}
