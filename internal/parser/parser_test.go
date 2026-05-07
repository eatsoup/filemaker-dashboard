package parser

import (
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want EventType
		db   string
		acct string
	}{
		{
			"conn open",
			`2026-05-03 15:50:00.473 +0200	Information	638	example.test	Client "Importer Script" opening a connection from "FileMaker Script" using "Server 21.1.7 [fmapp]".`,
			EventConnOpen, "", "",
		},
		{
			"db open",
			`2026-05-03 15:50:00.474 +0200	Information	94	example.test	Client "Importer Script (FileMaker Script)" opening database "Sales" as "import".`,
			EventDBOpen, "Sales", "import",
		},
		{
			"db close",
			`2026-05-03 15:50:00.693 +0200	Information	98	example.test	Client "Importer Script (FileMaker Script)" closing database "Sales" as "import".`,
			EventDBClose, "Sales", "import",
		},
		{
			"db open with host and ip",
			`2026-05-03 15:51:39.578 +0200	Information	94	example.test	Client "alice (WS-01) [192.0.2.10]" opening database "Inventory" as "alice".`,
			EventDBOpen, "Inventory", "alice",
		},
		{
			"conn close",
			`2026-05-03 15:55:38.679 +0200	Information	22	example.test	Client "alice (WS-01) [192.0.2.10]" closing a connection.`,
			EventConnClose, "", "",
		},
		{
			"auth fail",
			`2026-05-03 15:51:38.897 +0200	Information	730	example.test	SECURITY: Client "alice (WS-01) [192.0.2.10]" single sign-on authentication failed on database "Inventory.fmp12" using "alice [fmapp]".`,
			EventAuthFail, "", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := ParseLine(tc.line)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if ev == nil {
				t.Fatal("nil event")
			}
			if ev.Type != tc.want {
				t.Fatalf("type: got %v want %v", ev.Type, tc.want)
			}
			if ev.Database != tc.db {
				t.Fatalf("db: got %q want %q", ev.Database, tc.db)
			}
			if ev.Account != tc.acct {
				t.Fatalf("account: got %q want %q", ev.Account, tc.acct)
			}
		})
	}
}

func TestSplitClientID(t *testing.T) {
	cases := []struct {
		in, name, host, ip string
	}{
		{`alice (WS-01) [192.0.2.10]`, "alice", "WS-01", "192.0.2.10"},
		{`Importer Script (FileMaker Script)`, "Importer Script", "FileMaker Script", ""},
		{`bob (192.0.2.50) [192.0.2.50]`, "bob", "192.0.2.50", "192.0.2.50"},
		{`alice`, "alice", "", ""},
	}
	for _, c := range cases {
		n, h, ip := splitClientID(c.in)
		if n != c.name || h != c.host || ip != c.ip {
			t.Errorf("splitClientID(%q) = (%q,%q,%q), want (%q,%q,%q)", c.in, n, h, ip, c.name, c.host, c.ip)
		}
	}
}
