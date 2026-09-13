package validate

import (
	"testing"

	"github.com/saliheu/go-address-validator/internal/model"
	"github.com/saliheu/go-address-validator/internal/normalize"
)

// These cover the scoring and ranking logic, which is where the behaviour
// callers actually notice lives. The database paths are exercised separately
// by the integration script (scripts/smoke.sh) against a real Postgres, since
// the queries depend on Postgres-specific index behaviour and are not
// meaningfully testable against a stub.

func loc(id uint, name string, level int) model.Location {
	return model.Location{
		ID: id, Name: name, Level: level,
		NameKey: normalize.Key(name), CountryCode: "TR",
	}
}

var trLabels = map[int]string{1: "il", 2: "ilçe", 3: "yerleşim"}

func TestScoreAllRanksBestFirst(t *testing.T) {
	rows := []model.Location{
		loc(1, "Ankara", 1),
		loc(2, "Antalya", 1),
		loc(3, "Ankaralı", 2),
	}
	got := scoreAll(rows, normalize.Key("Ankara"), trLabels)

	if len(got) == 0 {
		t.Fatal("expected at least one scored row")
	}
	if got[0].Name != "Ankara" {
		t.Errorf("best match = %q, want %q", got[0].Name, "Ankara")
	}
	if got[0].Score != 1.0 {
		t.Errorf("exact match scored %v, want 1.0", got[0].Score)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Errorf("results not sorted descending: %v then %v",
				got[i-1].Score, got[i].Score)
		}
	}
}

func TestScoreAllDropsWeakMatches(t *testing.T) {
	rows := []model.Location{
		loc(1, "Ankara", 1),
		loc(2, "Zonguldak", 1), // nothing like the query
	}
	got := scoreAll(rows, normalize.Key("Ankara"), trLabels)
	for _, m := range got {
		if m.Score < SuggestThreshold {
			t.Errorf("%q kept with score %v, below SuggestThreshold %v",
				m.Name, m.Score, SuggestThreshold)
		}
		if m.Name == "Zonguldak" {
			t.Error("unrelated place should have been filtered out")
		}
	}
}

func TestScoreAllAttachesLevelLabel(t *testing.T) {
	got := scoreAll([]model.Location{loc(1, "Turgutlu", 2)}, normalize.Key("Turgutlu"), trLabels)
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].LevelLabel != "ilçe" {
		t.Errorf("level label = %q, want %q", got[0].LevelLabel, "ilçe")
	}
}

// A caller typing "Sisli" without diacritics must still match "Şişli".
func TestScoreAllIsDiacriticInsensitive(t *testing.T) {
	got := scoreAll([]model.Location{loc(1, "Şişli", 2)}, normalize.Key("Sisli"), trLabels)
	if len(got) != 1 || got[0].Score != 1.0 {
		t.Fatalf("diacritic-free input did not match exactly: %+v", got)
	}
}

// "Afyon" is how people write "Afyonkarahisar". It must clear AcceptThreshold,
// not merely appear as a suggestion.
func TestScoreAllAcceptsCommonTruncation(t *testing.T) {
	got := scoreAll([]model.Location{loc(1, "Afyonkarahisar", 1)}, normalize.Key("Afyon"), trLabels)
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].Score < AcceptThreshold {
		t.Errorf("truncation scored %v, want >= %v", got[0].Score, AcceptThreshold)
	}
}

func TestTopNRespectsLimitAndFloor(t *testing.T) {
	in := []Match{
		{Name: "a", Score: 0.95},
		{Name: "b", Score: 0.90},
		{Name: "c", Score: 0.70},
		{Name: "d", Score: 0.40}, // below any sensible floor
	}
	got := topN(in, 2, SuggestThreshold)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("got %v, want a,b", got)
	}

	all := topN(in, 10, SuggestThreshold)
	if len(all) != 3 {
		t.Errorf("len = %d, want 3 (the 0.40 entry must be cut)", len(all))
	}
}

func TestTopNStopsAtFirstBelowFloor(t *testing.T) {
	// topN relies on the input being sorted; it breaks rather than continues.
	in := []Match{{Score: 0.30}, {Score: 0.99}}
	if got := topN(in, 5, SuggestThreshold); len(got) != 0 {
		t.Errorf("expected early stop on sorted input, got %d", len(got))
	}
}

func TestFindByID(t *testing.T) {
	rows := []model.Location{loc(1, "Konak", 2), loc(7, "Bornova", 2)}
	if got := findByID(rows, 7); got == nil || got.Name != "Bornova" {
		t.Errorf("findByID(7) = %v, want Bornova", got)
	}
	if got := findByID(rows, 99); got != nil {
		t.Errorf("findByID(99) = %v, want nil", got)
	}
}

func TestRound2(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{1.0, 1.0}, {0.856, 0.86}, {0.854, 0.85}, {0.0, 0.0}, {0.999, 1.0},
	}
	for _, c := range cases {
		if got := round2(c.in); got != c.want {
			t.Errorf("round2(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Guard the invariant the thresholds depend on. If someone sets these the wrong
// way round, every weak match silently becomes a confirmed one.
func TestThresholdOrdering(t *testing.T) {
	if SuggestThreshold >= AcceptThreshold {
		t.Fatalf("SuggestThreshold (%v) must be below AcceptThreshold (%v)",
			SuggestThreshold, AcceptThreshold)
	}
}
