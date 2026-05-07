package parser

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type EventType int

const (
	EventUnknown EventType = iota
	EventConnOpen
	EventConnClose
	EventDBOpen
	EventDBClose
	EventAuthFail
)

// Event is one parsed line from the FileMaker access log.
type Event struct {
	Time     time.Time
	Code     string
	Server   string
	Type     EventType
	ClientID string // raw client identifier from `Client "..."`
	Name     string // client name without host/ip suffix
	Host     string // hostname, may be empty
	IP       string // ip address, may be empty
	Database string // db name (DBOpen/DBClose only)
	Account  string // login account (DBOpen/DBClose only)
	Version  string // e.g. "Pro 21.1.1"
	Protocol string // e.g. "fmapp", "fmrest"
}

// Line shape: "TIMESTAMP TZ\tLEVEL\tCODE\tSERVER\tMESSAGE"
// We split on tabs so the message body is one piece.
var (
	reConnOpen  = regexp.MustCompile(`^Client "(.+?)" opening a connection from "(.+?)" using "(.+?)"\.?$`)
	reConnClose = regexp.MustCompile(`^Client "(.+?)" closing a connection\.?$`)
	reDBOpen    = regexp.MustCompile(`^Client "(.+?)" opening database "(.+?)" as "(.+?)"\.?$`)
	reDBClose   = regexp.MustCompile(`^Client "(.+?)" closing database "(.+?)" as "(.+?)"\.?$`)
	reAuthFail  = regexp.MustCompile(`^SECURITY: Client "(.+?)" .* authentication failed`)

	// Used to split versioned protocol like "Pro 21.1.1 [fmapp]"
	reVersion = regexp.MustCompile(`^(.+?)\s*\[(.+?)\]\s*$`)

	// Split client identifier like `name (host) [ip]` or `name (FileMaker Script)`.
	// Captures: 1=name, 3=host, 5=ip
	reClientID = regexp.MustCompile(`^(.+?)(\s+\((.+?)\))?(\s+\[(.+?)\])?$`)
)

// ParseLine parses one log line. Returns nil event for blank/unrecognised lines.
func ParseLine(line string) (*Event, error) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, nil
	}
	parts := strings.SplitN(line, "\t", 5)
	if len(parts) < 5 {
		return nil, nil
	}
	tsRaw, _, code, server, msg := parts[0], parts[1], parts[2], parts[3], parts[4]

	ts, err := parseTimestamp(tsRaw)
	if err != nil {
		return nil, fmt.Errorf("timestamp %q: %w", tsRaw, err)
	}

	ev := &Event{Time: ts, Code: code, Server: server}

	switch {
	case reConnOpen.MatchString(msg):
		m := reConnOpen.FindStringSubmatch(msg)
		ev.Type = EventConnOpen
		ev.ClientID = m[1]
		ev.Name = m[1]
		host, ip := splitHostIP(m[2])
		ev.Host, ev.IP = host, ip
		v, p := splitVersion(m[3])
		ev.Version, ev.Protocol = v, p

	case reConnClose.MatchString(msg):
		m := reConnClose.FindStringSubmatch(msg)
		ev.Type = EventConnClose
		ev.ClientID = m[1]
		n, h, ip := splitClientID(m[1])
		ev.Name, ev.Host, ev.IP = n, h, ip

	case reDBOpen.MatchString(msg):
		m := reDBOpen.FindStringSubmatch(msg)
		ev.Type = EventDBOpen
		ev.ClientID = m[1]
		n, h, ip := splitClientID(m[1])
		ev.Name, ev.Host, ev.IP = n, h, ip
		ev.Database = m[2]
		ev.Account = m[3]

	case reDBClose.MatchString(msg):
		m := reDBClose.FindStringSubmatch(msg)
		ev.Type = EventDBClose
		ev.ClientID = m[1]
		n, h, ip := splitClientID(m[1])
		ev.Name, ev.Host, ev.IP = n, h, ip
		ev.Database = m[2]
		ev.Account = m[3]

	case reAuthFail.MatchString(msg):
		m := reAuthFail.FindStringSubmatch(msg)
		ev.Type = EventAuthFail
		ev.ClientID = m[1]
		n, h, ip := splitClientID(m[1])
		ev.Name, ev.Host, ev.IP = n, h, ip

	default:
		ev.Type = EventUnknown
	}

	return ev, nil
}

// splitClientID parses "name (host) [ip]" / "name (FileMaker Script)" / "name".
func splitClientID(s string) (name, host, ip string) {
	m := reClientID.FindStringSubmatch(s)
	if m == nil {
		return s, "", ""
	}
	return m[1], m[3], m[5]
}

// splitHostIP parses "HOSTNAME (IP)" or "IP (IP)" or just "IP" or "FileMaker Script".
func splitHostIP(s string) (host, ip string) {
	if i := strings.LastIndex(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		return s[:i], s[i+2 : len(s)-1]
	}
	return s, ""
}

// splitVersion parses "Pro 21.1.1 [fmapp]" → version="Pro 21.1.1", proto="fmapp"
func splitVersion(s string) (version, proto string) {
	m := reVersion.FindStringSubmatch(s)
	if m == nil {
		return s, ""
	}
	return strings.TrimSpace(m[1]), m[2]
}

// FileMaker timestamps look like "2026-05-03 15:40:00.517 +0200".
func parseTimestamp(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.000 -0700",
		"2006-01-02 15:04:05 -0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp")
}
