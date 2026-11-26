// Package frontmigration converts retired front page settings to ordered sections.
package frontmigration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Section struct {
	Type      string   `json:"type"`
	Pages     []string `json:"pages,omitempty"`
	Tag       string   `json:"tag,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	Title     string   `json:"title,omitempty"`
	Layout    string   `json:"layout,omitempty"`
	LinkURL   string   `json:"link_url,omitempty"`
	LinkLabel string   `json:"link_label,omitempty"`
}

// Convert maps legacy settings to the ordered front_sections representation.
func Convert(values map[string]string) ([]Section, error) {
	var cards []string
	if raw := strings.TrimSpace(values["front_cards"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cards); err != nil {
			return nil, fmt.Errorf("front_cards: %w", err)
		}
	}
	var out []Section
	if strings.TrimSpace(values["front_page"]) != "" {
		out = append(out, Section{Type: "hero"})
	}
	if len(cards) > 0 {
		out = append(out, Section{Type: "cards", Pages: cards})
	}
	tag := strings.TrimSpace(values["top_gallery_tag"])
	if tag != "" {
		limit := 6
		if n, err := strconv.Atoi(values["top_gallery_limit"]); err == nil && n > 0 {
			limit = n
		}
		out = append(out, Section{Type: "gallery", Tag: tag, Limit: limit, Title: strings.TrimSpace(values["top_gallery_title"]), Layout: strings.TrimSpace(values["top_gallery_layout"]), LinkURL: strings.TrimSpace(values["top_gallery_link_url"]), LinkLabel: strings.TrimSpace(values["top_gallery_link_label"])})
	}
	out = append(out, Section{Type: "article_list"})
	return out, nil
}

// Migrate converts stored legacy settings transactionally. emit receives the dry-run plan when dryRun is true.
func Migrate(ctx context.Context, db *sql.DB, dryRun bool, emit func(string)) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN ('front_page','front_cards','top_gallery_tag','top_gallery_limit','top_gallery_title','top_gallery_layout','top_gallery_link_url','top_gallery_link_label','front_sections')`)
	if err != nil {
		return err
	}
	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		values[k] = v
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	legacyPresent := false
	for _, key := range []string{"front_cards", "top_gallery_tag", "top_gallery_limit", "top_gallery_title", "top_gallery_layout", "top_gallery_link_url", "top_gallery_link_label"} {
		if _, ok := values[key]; ok {
			legacyPresent = true
			break
		}
	}
	if _, exists := values["front_sections"]; exists && !legacyPresent {
		if dryRun && emit != nil {
			emit("front_sections already exists; no changes")
		}
		return nil
	}
	sections, err := Convert(values)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(sections)
	if err != nil {
		return err
	}
	if dryRun {
		if emit != nil {
			emit(fmt.Sprintf("would write front_sections=%s and remove legacy keys", encoded))
		}
		return nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('front_sections',?,CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=CURRENT_TIMESTAMP`, string(encoded)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key IN ('front_cards','top_gallery_tag','top_gallery_limit','top_gallery_title','top_gallery_layout','top_gallery_link_url','top_gallery_link_label')`); err != nil {
		return err
	}
	return tx.Commit()
}
