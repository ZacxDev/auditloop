package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// mapNoRows normalizes sql.ErrNoRows to the package ErrNotFound sentinel.
func mapNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// nowRFC returns the current UTC time as RFC3339Nano (timestamps are stored as
// TEXT for cross-dialect portability).
func nowRFC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func toRFC(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// parseTime parses a stored RFC3339 timestamp (best-effort; zero on failure).
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	// SQLite may return a naive "2006-01-02 15:04:05" for CURRENT_TIMESTAMP; we
	// store RFC3339 ourselves, but be lenient.
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}

// parseTimePtr returns nil for NULL/empty, else a pointer.
func parseTimePtr(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}

func marshalStrings(ss []string) string {
	if ss == nil {
		ss = []string{}
	}
	b, _ := json.Marshal(ss)
	return string(b)
}

func unmarshalStrings(s string) []string {
	var out []string
	if s == "" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}
