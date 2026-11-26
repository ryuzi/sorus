package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertLegacySettings(t *testing.T) {
	got, err := convert(map[string]string{"front_page": " research ", "front_cards": `["access"]`, "top_gallery_tag": " campus ", "top_gallery_limit": "0", "top_gallery_title": " Photos "})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Type != "hero" || got[1].Type != "cards" || got[2].Type != "gallery" || got[2].Limit != 6 || got[3].Type != "article_list" {
		t.Fatalf("converted sections = %#v", got)
	}
	if got[2].Title != "Photos" {
		t.Fatalf("gallery title = %q", got[2].Title)
	}
}

func TestRunDryRunAndExistingDefaultSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at TEXT); INSERT INTO settings VALUES('front_sections','[]','now'),('front_page','research','now'),('front_cards','["access"]','now'),('top_gallery_tag','campus','now')`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := run(context.Background(), path, true); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var before string
	if err = db.QueryRow(`SELECT value FROM settings WHERE key='front_sections'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != "[]" {
		t.Fatalf("dry-run wrote front_sections: %s", before)
	}
	if err := run(context.Background(), path, false); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = db.QueryRow(`SELECT value FROM settings WHERE key='front_sections'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var sections []section
	if err = json.Unmarshal([]byte(raw), &sections); err != nil {
		t.Fatal(err)
	}
	if len(sections) != 4 || sections[0].Type != "hero" || sections[1].Type != "cards" || sections[2].Type != "gallery" || sections[3].Type != "article_list" {
		t.Fatalf("migrated sections = %s", raw)
	}
	var legacy int
	if err = db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key IN ('front_cards','top_gallery_tag')`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Fatalf("legacy keys remain: %d", legacy)
	}
	if err := run(context.Background(), path, false); err != nil {
		t.Fatal(err)
	}
}

func TestConvertRejectsInvalidCardsJSON(t *testing.T) {
	_, err := convert(map[string]string{"front_cards": "not json"})
	if err == nil || !strings.Contains(err.Error(), "front_cards") {
		t.Fatalf("error = %v", err)
	}
}
