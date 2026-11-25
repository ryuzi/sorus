package i18n

import "testing"

func TestTranslate(t *testing.T) {
	if got := Translate("ja", "dashboard"); got != "ダッシュボード" {
		t.Fatalf("unexpected translation: %q", got)
	}
	if got := Translate("en", "dashboard"); got != "Dashboard" {
		t.Fatalf("unexpected translation: %q", got)
	}
	if got := Translate("fr", "dashboard"); got != "ダッシュボード" {
		t.Fatalf("fallback to ja failed: %q", got)
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("EN"); got != EN {
		t.Fatalf("expected en, got %q", got)
	}
	if got := Normalize("en-US"); got != EN {
		t.Fatalf("expected en for en-US, got %q", got)
	}
	if got := Normalize("ja"); got != JA {
		t.Fatalf("expected ja, got %q", got)
	}
}

func TestCatalogKeyParity(t *testing.T) {
	jaMap := catalog[JA]
	enMap := catalog[EN]

	for k := range jaMap {
		if _, ok := enMap[k]; !ok {
			t.Errorf("key %q is in JA catalog but missing in EN catalog", k)
		}
	}
	for k := range enMap {
		if _, ok := jaMap[k]; !ok {
			t.Errorf("key %q is in EN catalog but missing in JA catalog", k)
		}
	}
}
