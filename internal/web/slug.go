package web

import (
	"regexp"
	"strings"
)

// slugPattern is the registry-id shape shared by game ids and node ids: a
// lowercase slug that starts with an alphanumeric and contains only
// [a-z0-9-] thereafter (e.g. "super-metroid", "bob-deck"). It deliberately
// forbids a leading/trailing/uppercase form so ids stay URL- and
// filesystem-clean across both the games and nodes registries.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validSlug reports whether id is a well-formed registry slug.
func validSlug(id string) bool {
	return slugPattern.MatchString(id)
}

// slugify derives a slug from a free-form display string: lowercase, spaces (and
// runs of whitespace/underscores) to single hyphens, every other non-[a-z0-9-]
// rune dropped, and collapsed/trimmed hyphens. Used to auto-generate a game id
// from its display when the admin leaves the id field blank (docs/open-questions.md:
// "auto with manual override"). The result is not guaranteed non-empty (a display
// of only punctuation slugs to ""), so callers must still validSlug the output.
func slugify(display string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(display)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == ' ' || r == '\t' || r == '\n' || r == '_' || r == '-':
			// Collapse any run of separators into a single hyphen.
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		default:
			// Drop every other rune (punctuation, symbols, non-ASCII).
		}
	}
	return strings.Trim(b.String(), "-")
}
