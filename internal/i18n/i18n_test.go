package i18n

import (
	"strings"
	"testing"
)

// TestCatalogComplete is the catalog lint: every key must carry BOTH languages
// so Spanish never silently degrades to English.
func TestCatalogComplete(t *testing.T) {
	for key, e := range catalog {
		if strings.TrimSpace(e.en) == "" {
			t.Errorf("key %q missing English", key)
		}
		if strings.TrimSpace(e.es) == "" {
			t.Errorf("key %q missing Spanish", key)
		}
	}
}

func TestTranslation(t *testing.T) {
	t.Cleanup(func() { current = EN })

	SetLanguage("en")
	if got := T("engine.completed"); got != "run completed" {
		t.Errorf("en: %q", got)
	}
	SetLanguage("es")
	if got := T("engine.completed"); got != "run completado" {
		t.Errorf("es: %q", got)
	}
	if got := T("engine.stage", "verify"); got != "etapa: verify" {
		t.Errorf("es with args: %q", got)
	}
	// Invalid language keeps the current one.
	SetLanguage("fr")
	if Language() != ES {
		t.Error("invalid language must not change selection")
	}
}

func TestUnknownKeyIsVisible(t *testing.T) {
	if got := T("no.such.key"); got != "no.such.key" {
		t.Errorf("unknown key should surface itself, got %q", got)
	}
}
