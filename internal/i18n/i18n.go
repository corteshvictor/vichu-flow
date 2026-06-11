// Package i18n provides the UI string catalog. English is the default; Spanish
// is first-class. Every user-facing UI string goes through T() from the first
// commit so no retrofit is ever needed.
//
// Scope note: only *UI* strings are translated. Runtime files (state.json,
// events.ndjson, blocked reasons) are machine-readable data and stay English so
// external tooling can rely on them; views translate labels around them.
package i18n

import (
	"fmt"
	"os"
)

// Lang is a supported UI language.
type Lang string

const (
	EN Lang = "en"
	ES Lang = "es"
)

var current = defaultLang()

func defaultLang() Lang {
	if os.Getenv("VICHU_LANG") == "es" {
		return ES
	}
	return EN
}

// SetLanguage selects the UI language ("en" or "es"); anything else keeps the
// current language.
func SetLanguage(s string) {
	switch s {
	case "en":
		current = EN
	case "es":
		current = ES
	}
}

// Language returns the active UI language.
func Language() Lang { return current }

// T renders the catalog entry for key in the active language, applying
// fmt.Sprintf when args are given. Unknown keys return the key itself (visible
// in output and caught by the catalog test) rather than failing.
func T(key string, args ...any) string {
	entry, ok := catalog[key]
	if !ok {
		return key
	}
	tmpl := entry.en
	if current == ES && entry.es != "" {
		tmpl = entry.es
	}
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}

type entry struct {
	en string
	es string
}
