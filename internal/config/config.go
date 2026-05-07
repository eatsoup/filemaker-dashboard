package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	LogfilePath    string        `yaml:"logfile_path"`
	DBPath         string        `yaml:"db_path"`
	ListenAddr     string        `yaml:"listen_addr"`
	IngestInterval time.Duration `yaml:"ingest_interval"`
	SessionTTL     time.Duration `yaml:"session_ttl"`
	// AbandonedSessionAfter is the age past which a still-open FileMaker
	// session is presumed silently disconnected and force-closed by the
	// ingester. FileMaker Server occasionally drops sessions without
	// writing a "closing database" line; without this, those rows would
	// stay end_time=NULL forever and never appear in usage totals.
	AbandonedSessionAfter time.Duration `yaml:"abandoned_session_after"`
	InitialAdmin          struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"initial_admin"`
	Defaults Defaults `yaml:"defaults"`
}

// Defaults are optional UI defaults applied to the dashboard and report forms
// on initial page load. Once the user submits a form, the URL is authoritative.
type Defaults struct {
	MinDuration      int      `yaml:"min_duration"`
	MinUsers         int      `yaml:"min_users"`
	GroupBy          string   `yaml:"group_by"`
	ExcludeUsers     []string `yaml:"exclude_users"`
	ExcludeDatabases []string `yaml:"exclude_databases"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := &Config{
		DBPath:                "filemaker.db",
		ListenAddr:            ":8080",
		IngestInterval:        10 * time.Minute,
		SessionTTL:            7 * 24 * time.Hour,
		AbandonedSessionAfter: 12 * time.Hour,
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.LogfilePath == "" {
		return nil, fmt.Errorf("logfile_path is required")
	}
	if c.InitialAdmin.Username == "" || c.InitialAdmin.Password == "" {
		return nil, fmt.Errorf("initial_admin.username and initial_admin.password are required")
	}
	return c, nil
}
