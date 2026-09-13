package normalize

import (
	"strings"
	"testing"
)

func TestTurkishLower(t *testing.T) {
	cases := []struct{ in, want string }{
		{"İSTANBUL", "istanbul"},
		{"IĞDIR", "ığdır"},
		{"Iğdır", "ığdır"},
		{"İzmir", "izmir"},
		{"ŞANLIURFA", "şanlıurfa"},
		{"ÇANAKKALE", "çanakkale"},
		{"MUĞLA", "muğla"},
		{"BERLIN", "berlın"}, // ASCII I always maps to ı: this is a Turkish-locale lowercaser
	}
	for _, c := range cases {
		if got := TurkishLower(c.in); got != c.want {
			t.Errorf("TurkishLower(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Go's strings.ToLower is locale-independent and loses the dotless i.
// This pins the exact gap TurkishLower exists to close.
func TestTurkishLowerBeatsStdlib(t *testing.T) {
	if strings.ToLower("IĞDIR") == "ığdır" {
		t.Skip("stdlib became Turkish-aware; TurkishLower may be unnecessary")
	}
	if got := TurkishLower("IĞDIR"); got != "ığdır" {
		t.Errorf("TurkishLower(%q) = %q, want %q", "IĞDIR", got, "ığdır")
	}
}

// Input lowercased by a JavaScript client contains i + U+0307 for "İ".
// Key must fold that to the same value as plain ASCII input.
func TestKeyStripsCombiningDot(t *testing.T) {
	fromJS := "i̇stanbul" // what "İSTANBUL".toLowerCase() yields in JS
	if got, want := Key(fromJS), Key("Istanbul"); got != want {
		t.Errorf("Key(JS-lowercased) = %q, want %q", got, want)
	}
}

func TestKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  İSTANBUL ", "istanbul"},
		{"Şişli/İstanbul", "sisli istanbul"},
		{"St. John's", "st johns"},
		{"Afyonkarahisar", "afyonkarahisar"},
		{"IĞDIR", "igdir"},
		{"Çanakkale", "canakkale"},
		{"NEW    YORK", "new york"},
		{"Frankfurt am Main", "frankfurt am main"},
		{"Köln", "koln"},
		{"", ""},
		{"   ", ""},
		{"---", ""},
	}
	for _, c := range cases {
		if got := Key(c.in); got != c.want {
			t.Errorf("Key(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Diacritics must not change the key: this is the whole point of folding.
func TestKeyIsDiacriticInsensitive(t *testing.T) {
	pairs := [][2]string{
		{"Şişli", "Sisli"},
		{"Muğla", "Mugla"},
		{"Çorum", "Corum"},
		{"Iğdır", "Igdir"},
		{"Köln", "Koln"},
		{"Malmö", "Malmo"},
	}
	for _, p := range pairs {
		if Key(p[0]) != Key(p[1]) {
			t.Errorf("Key(%q)=%q != Key(%q)=%q", p[0], Key(p[0]), p[1], Key(p[1]))
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"kitten", "sitting", 3},
		{"istanbul", "istanbul", 0},
		{"ankara", "ankarra", 1},
		{"izmir", "izmit", 1},
	}
	for _, c := range cases {
		if got := Levenshtein([]rune(c.a), []rune(c.b)); got != c.want {
			t.Errorf("Levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLevenshteinIsSymmetric(t *testing.T) {
	pairs := [][2]string{{"ankara", "ankarra"}, {"izmir", "istanbul"}, {"a", "xyz"}}
	for _, p := range pairs {
		ab := Levenshtein([]rune(p[0]), []rune(p[1]))
		ba := Levenshtein([]rune(p[1]), []rune(p[0]))
		if ab != ba {
			t.Errorf("asymmetric for %q/%q: %d vs %d", p[0], p[1], ab, ba)
		}
	}
}

func TestSimilarityExactAndEmpty(t *testing.T) {
	if got := Similarity("istanbul", "istanbul"); got != 1.0 {
		t.Errorf("exact match = %v, want 1.0", got)
	}
	if got := Similarity("", "istanbul"); got != 0.0 {
		t.Errorf("empty input = %v, want 0.0", got)
	}
}

// A truncated name ("afyon" for "afyonkarahisar") must stay well above the
// accept threshold. Plain edit distance would score this 0.50.
func TestSimilarityRewardsPrefix(t *testing.T) {
	got := Similarity("afyon", "afyonkarahisar")
	if got < 0.85 {
		t.Errorf("prefix match scored %v, want >= 0.85", got)
	}
	if got >= 1.0 {
		t.Errorf("prefix match scored %v, must stay below an exact match", got)
	}
}

// Two-letter prefixes are far too weak to trust; the rule requires 4+ runes.
func TestSimilarityIgnoresShortPrefix(t *testing.T) {
	if got := Similarity("is", "istanbul"); got >= 0.85 {
		t.Errorf("short prefix scored %v, want < 0.85", got)
	}
}

func TestSimilarityTypoTolerance(t *testing.T) {
	// one typo in a long name should still be a confident match
	if got := Similarity("ankarra", "ankara"); got < 0.80 {
		t.Errorf("single typo scored %v, want >= 0.80", got)
	}
	// genuinely different places must not be confused
	if got := Similarity("izmir", "istanbul"); got > 0.60 {
		t.Errorf("unrelated names scored %v, want <= 0.60", got)
	}
}

// izmir/izmit are one edit apart but are different cities. This documents that
// the scorer alone cannot separate them — the caller must disambiguate with the
// parent region. Kept as a test so the limitation stays visible.
func TestSimilarityKnownAmbiguity(t *testing.T) {
	got := Similarity("izmir", "izmit")
	if got < 0.75 {
		t.Fatalf("izmir/izmit scored %v; expected a high, ambiguous score", got)
	}
	t.Logf("known ambiguity: izmir vs izmit = %.2f (disambiguate by admin1)", got)
}

func BenchmarkKey(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Key("Şişli/İstanbul")
	}
}

func BenchmarkSimilarity(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Similarity("afyonkarahisar", "afyonkarahsar")
	}
}
