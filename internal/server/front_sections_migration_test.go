package server

import (
	"encoding/json"
	"strings"
	"testing"

	"sorus/internal/frontmigration"
)

// TestFrontSectionsMigrationGolden preserves the old research-site four-part homepage contract:
// hero, cards, gallery, then news.  The legacy settings are translated by the one-off CLI into
// this ordered model; this test locks the resulting public HTML landmarks and their order.
func TestFrontSectionsMigrationGolden(t *testing.T) {
	env := newFrontEnv(t)
	env.insertMedia("migration-gallery", "migration-gallery.webp", "研究棟", "春の研究棟", "学校")
	legacySettings := map[string]string{
		"front_page": "research", "front_cards": `["access"]`, "top_gallery_tag": "学校",
		"top_gallery_limit": "4", "top_gallery_title": "キャンパスの写真", "top_gallery_layout": galleryLayoutJustified,
	}
	for key, value := range legacySettings {
		if _, err := env.db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES(?,?,CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES('front_sections','[]',CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value='[]'`); err != nil {
		t.Fatal(err)
	}
	if err := frontmigration.Migrate(t.Context(), env.db.DB, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"front_cards", "top_gallery_tag", "top_gallery_limit", "top_gallery_title", "top_gallery_layout", "top_gallery_link_url", "top_gallery_link_label"} {
		var count int
		if err := env.db.Get(&count, `SELECT COUNT(*) FROM settings WHERE key = ?`, key); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("legacy setting %q remains", key)
		}
	}
	var migratedJSON string
	if err := env.db.Get(&migratedJSON, `SELECT value FROM settings WHERE key='front_sections'`); err != nil {
		t.Fatal(err)
	}
	var sections []FrontSection
	if err := json.Unmarshal([]byte(migratedJSON), &sections); err != nil {
		t.Fatal(err)
	}
	if err := env.server.saveSiteStructure(t.Context(), SiteStructure{FrontPage: legacySettings["front_page"], FrontSections: sections}); err != nil {
		t.Fatal(err)
	}
	body := env.newClient().get("/").Body
	golden := []string{
		`<section class="front-hero" aria-labelledby="front-hero-title">`,
		`<h1 id="front-hero-title" class="article-title">研究紹介</h1>`,
		`<ul class="front-cards">`,
		`<li class="front-card`,
		`<h2 class="front-card-title"><a href="/access">アクセス</a></h2>`,
		`<section class="front-gallery" aria-labelledby="front-gallery-title">`,
		`class="photo-gallery gallery-layout-justified" data-gallery-layout="justified" data-gallery-tag="学校"`,
		`alt="研究棟"`,
		`<section class="front-news" aria-labelledby="front-news-title">`,
		`<h2 id="front-news-title" class="page-title">新着記事</h2>`,
		`<div class="card-grid`,
	}
	last := -1
	for _, fragment := range golden {
		pos := strings.Index(body, fragment)
		if pos < 0 {
			t.Errorf("golden HTML is missing %q", fragment)
			continue
		}
		if pos <= last {
			t.Errorf("golden HTML fragment %q is out of section order", fragment)
		}
		last = pos
	}
}
