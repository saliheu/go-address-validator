// Package normalize turns messy, human-typed place names into a canonical form
// that can be compared reliably.
//
// The hard part here is Turkish. Go's strings.ToLower applies *simple* Unicode
// case mapping, which is locale-independent, so it gets the dotless i wrong:
//
//	strings.ToLower("IĞDIR") => "iğdir"   // Turkish requires "ığdır"
//
// The dotless ı is lost, so user input "IĞDIR" no longer matches a record
// stored as "Iğdır". (The other direction is fine in Go: "İ" maps to a plain
// "i" with no combining mark.)
//
// A second hazard arrives from upstream. Runtimes that apply *full* Unicode
// case mapping — JavaScript and Java among them — lowercase U+0130 (İ) to
// "i" + U+0307 COMBINING DOT ABOVE. That extra rune is invisible when printed
// but makes string equality fail. Since this service accepts input from
// browser clients, Key strips combining marks defensively.
//
// Both problems are silent: nothing errors, matches just quietly disappear.
package normalize

import (
	"strings"
	"unicode"
)

// TurkishLower lowercases using Turkish rules: İ→i, I→ı. Every other rune
// falls back to the default Unicode mapping.
func TurkishLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 'İ': // U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE
			b.WriteRune('i')
		case 'I': // U+0049 LATIN CAPITAL LETTER I
			b.WriteRune('ı')
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// foldMap collapses accented Latin letters onto their ASCII base. This is
// deliberately lossy: "Şişli" and "Sisli" must compare equal, because users
// type both and many upstream systems strip diacritics anyway.
var foldMap = map[rune]rune{
	'ç': 'c', 'ğ': 'g', 'ı': 'i', 'ö': 'o', 'ş': 's', 'ü': 'u',
	'â': 'a', 'î': 'i', 'û': 'u', 'à': 'a', 'á': 'a', 'ä': 'a', 'å': 'a',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'ï': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ø': 'o',
	'ù': 'u', 'ú': 'u',
	'ñ': 'n', 'ý': 'y', 'ÿ': 'y', 'ß': 's', 'æ': 'a',
	'ż': 'z', 'ź': 'z', 'ł': 'l', 'ć': 'c', 'ś': 's', 'ń': 'n',
	'ě': 'e', 'š': 's', 'č': 'c', 'ř': 'r', 'ž': 'z', 'ů': 'u',
}

// Key produces the canonical comparison key for a place name:
// Turkish-aware lowercase, diacritics folded, punctuation dropped,
// internal whitespace collapsed to a single space.
//
//	Key("  İSTANBUL ")      == "istanbul"
//	Key("Şişli/İstanbul")   == "sisli istanbul"
//	Key("St. John's")       == "st johns"
func Key(s string) string {
	lowered := TurkishLower(s)

	var b strings.Builder
	b.Grow(len(lowered))
	lastWasSpace := true // leading spaces are dropped
	for _, r := range lowered {
		// Drop combining marks. Input lowercased by a JavaScript client carries
		// U+0307 after a dotted capital I; it is invisible but breaks equality.
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if folded, ok := foldMap[r]; ok {
			r = folded
		}
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastWasSpace = false
		case unicode.IsSpace(r) || r == '-' || r == '/' || r == ',':
			// treat separators as word boundaries
			if !lastWasSpace {
				b.WriteRune(' ')
				lastWasSpace = true
			}
		default:
			// drop apostrophes, dots, parentheses etc. entirely so that
			// "St. John's" and "St Johns" collapse to the same key
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// Levenshtein returns the edit distance between two rune slices using the
// two-row variant: O(n*m) time but only O(min(n,m)) memory.
func Levenshtein(a, b []rune) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(
				curr[j-1]+1,    // insertion
				prev[j]+1,      // deletion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// Similarity scores two already-normalized keys in [0,1].
//
// It is not pure edit distance. Two behaviours matter for addresses:
//
//   - A prefix match should score high. People truncate ("Afyon" for
//     "Afyonkarahisar"), and raw Levenshtein punishes that harshly:
//     8 edits out of 16 characters would score 0.50.
//   - Distance must be scaled by length. One typo in a 4-letter town is a
//     much weaker signal than one typo in a 20-letter one.
func Similarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	if a == "" || b == "" {
		return 0.0
	}

	ra, rb := []rune(a), []rune(b)
	longer, shorter := ra, rb
	if len(longer) < len(shorter) {
		longer, shorter = shorter, longer
	}

	// Prefix rule: the shorter string is a leading substring of the longer one.
	if len(shorter) >= 4 && strings.HasPrefix(string(longer), string(shorter)) {
		coverage := float64(len(shorter)) / float64(len(longer))
		// Floor at 0.85 so a legitimate abbreviation stays clearly above the
		// default accept threshold, while still ranking below an exact match.
		return 0.85 + 0.15*coverage
	}

	dist := Levenshtein(ra, rb)
	return 1.0 - float64(dist)/float64(len(longer))
}
