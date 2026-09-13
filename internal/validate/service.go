// Package validate implements the matching logic behind the public endpoints.
//
// Matching runs in two stages on purpose:
//
//  1. Exact key lookup. This is a single indexed equality read and covers the
//     large majority of real traffic, because most callers send clean input.
//  2. Only on a miss, a bounded fuzzy pass. Candidates are narrowed with an
//     indexed prefix scan first, then scored in Go.
//
// The alternative - scoring every row in the country on every request - is
// simpler to write and unusable in production. Turkey alone has ~50k populated
// places in GeoNames.
package validate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"

	"github.com/saliheu/go-address-validator/internal/model"
	"github.com/saliheu/go-address-validator/internal/normalize"
)

// Thresholds for turning a similarity score into a verdict.
const (
	// AcceptThreshold: at or above this, the match is returned as valid.
	AcceptThreshold = 0.85
	// SuggestThreshold: below Accept but at or above this, the match is
	// returned as a suggestion rather than a confirmation.
	SuggestThreshold = 0.65
	// prefixLen is how many leading runes the candidate scan keys on. Three is
	// a deliberate compromise: shorter pulls too many rows, longer misses
	// typos in the first characters ("Antalia" vs "Antalya" is fine, "Ankra"
	// vs "Ankara" is not).
	prefixLen = 3
	// maxCandidates caps the fuzzy pass so one pathological query cannot
	// dominate a worker.
	maxCandidates = 400
)

var ErrUnknownCountry = errors.New("unknown country code")

type Service struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Service { return &Service{db: db} }

// ---------- public shapes ----------

type Match struct {
	ID         uint    `json:"id"`
	Name       string  `json:"name"`
	Level      int     `json:"level"`
	LevelLabel string  `json:"level_label,omitempty"`
	Score      float64 `json:"score"`
}

// HierarchyNode is one ancestor of a match, named in the country's own terms.
type HierarchyNode struct {
	Level      int    `json:"level"`
	LevelLabel string `json:"level_label,omitempty"`
	Name       string `json:"name"`
}

type CityRequest struct {
	Country string // ISO-3166-1 alpha-2
	City    string
	Region  string // optional parent filter (il / state / province)
}

type CityResponse struct {
	Valid       bool            `json:"valid"`
	Input       string          `json:"input"`
	Country     string          `json:"country"`
	Match       *Match          `json:"match,omitempty"`
	Hierarchy   []HierarchyNode `json:"hierarchy,omitempty"`
	Suggestions []Match         `json:"suggestions,omitempty"`
	Reason      string          `json:"reason,omitempty"`
}

type ZipRequest struct {
	Country string
	Code    string
	City    string // optional cross-check
}

type ZipResponse struct {
	Valid       bool     `json:"valid"`
	Input       string   `json:"input"`
	Country     string   `json:"country"`
	Code        string   `json:"code,omitempty"`
	Places      []string `json:"places,omitempty"`
	Admin1      string   `json:"admin1,omitempty"`
	Admin2      string   `json:"admin2,omitempty"`
	CityMatches *bool    `json:"city_matches,omitempty"`
	Score       float64  `json:"score"`
	Reason      string   `json:"reason,omitempty"`
}

// ---------- city validation ----------

func (s *Service) ValidateCity(ctx context.Context, req CityRequest) (*CityResponse, error) {
	country := strings.ToUpper(strings.TrimSpace(req.Country))
	resp := &CityResponse{Input: req.City, Country: country}

	if err := s.assertCountry(ctx, country); err != nil {
		return nil, err
	}
	key := normalize.Key(req.City)
	if key == "" {
		resp.Reason = "empty city name"
		return resp, nil
	}

	labels, err := s.levelLabels(ctx, country)
	if err != nil {
		return nil, err
	}

	// Optional parent scope. Resolving the region first is what disambiguates
	// same-named places - Turkey has more than one "Merkez" district, and the
	// only thing separating them is the province above.
	var parentIDs []uint
	if strings.TrimSpace(req.Region) != "" {
		parentIDs, err = s.resolveRegion(ctx, country, req.Region)
		if err != nil {
			return nil, err
		}
		if len(parentIDs) == 0 {
			resp.Reason = fmt.Sprintf("region %q not found in %s", req.Region, country)
			return resp, nil
		}
	}

	// Stage 1: exact.
	exact, err := s.lookupExact(ctx, country, key, parentIDs)
	if err != nil {
		return nil, err
	}
	if exact != nil {
		resp.Valid = true
		resp.Match = &Match{
			ID: exact.ID, Name: exact.Name, Level: exact.Level,
			LevelLabel: labels[exact.Level], Score: 1.0,
		}
		resp.Hierarchy, err = s.ancestry(ctx, exact, labels)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}

	// Stage 2: fuzzy.
	candidates, err := s.candidates(ctx, country, key, parentIDs)
	if err != nil {
		return nil, err
	}
	scored := scoreAll(candidates, key, labels)
	if len(scored) == 0 {
		resp.Reason = "no similar place found"
		return resp, nil
	}

	best := scored[0]
	if best.Score >= AcceptThreshold {
		resp.Valid = true
		resp.Match = &best
		if loc := findByID(candidates, best.ID); loc != nil {
			if resp.Hierarchy, err = s.ancestry(ctx, loc, labels); err != nil {
				return nil, err
			}
		}
		if len(scored) > 1 {
			resp.Suggestions = topN(scored[1:], 3, SuggestThreshold)
		}
		return resp, nil
	}

	resp.Reason = "no confident match"
	resp.Suggestions = topN(scored, 5, SuggestThreshold)
	return resp, nil
}

// ---------- postcode validation ----------

func (s *Service) ValidateZipcode(ctx context.Context, req ZipRequest) (*ZipResponse, error) {
	country := strings.ToUpper(strings.TrimSpace(req.Country))
	resp := &ZipResponse{Input: req.Code, Country: country}

	if err := s.assertCountry(ctx, country); err != nil {
		return nil, err
	}
	codeKey := normalize.Key(req.Code)
	// Postcodes are compared without internal spaces: UK "SW1A 1AA" and
	// "SW1A1AA" are the same code written two ways.
	codeKey = strings.ReplaceAll(codeKey, " ", "")
	if codeKey == "" {
		resp.Reason = "empty postal code"
		return resp, nil
	}

	var rows []model.PostalCode
	err := s.db.WithContext(ctx).
		Where("country_code = ? AND code_key = ?", country, codeKey).
		Limit(50).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("postal lookup: %w", err)
	}
	if len(rows) == 0 {
		resp.Reason = "postal code not found for country"
		return resp, nil
	}

	resp.Valid = true
	resp.Score = 1.0
	resp.Code = rows[0].Code
	resp.Admin1 = rows[0].Admin1Name
	resp.Admin2 = rows[0].Admin2Name

	seen := map[string]bool{}
	for _, r := range rows {
		if r.PlaceName != "" && !seen[r.PlaceName] {
			resp.Places = append(resp.Places, r.PlaceName)
			seen[r.PlaceName] = true
		}
	}

	// Cross-check: does the supplied city actually sit under this code? A code
	// that exists is not the same as a code that matches the rest of the
	// address, and conflating the two is how bad addresses get accepted.
	if strings.TrimSpace(req.City) != "" {
		cityKey := normalize.Key(req.City)
		bestScore := 0.0
		for _, r := range rows {
			if sc := normalize.Similarity(cityKey, r.PlaceKey); sc > bestScore {
				bestScore = sc
			}
		}
		matches := bestScore >= AcceptThreshold
		resp.CityMatches = &matches
		resp.Score = bestScore
		if !matches {
			resp.Valid = false
			resp.Reason = fmt.Sprintf(
				"postal code exists but does not cover %q (best %.2f)", req.City, bestScore)
		}
	}
	return resp, nil
}

// ---------- internals ----------

// assertCountry rejects codes the service holds no data for. Without this a
// typo in the country code silently degrades to "no match found", which reads
// to the caller like the city is wrong rather than the request being malformed.
func (s *Service) assertCountry(ctx context.Context, country string) error {
	if len(country) != 2 {
		return fmt.Errorf("%w: %q (expected ISO-3166-1 alpha-2)", ErrUnknownCountry, country)
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&model.Country{}).
		Where("code = ?", country).Count(&count).Error; err != nil {
		return fmt.Errorf("country lookup: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: %q", ErrUnknownCountry, country)
	}
	return nil
}

func (s *Service) levelLabels(ctx context.Context, country string) (map[int]string, error) {
	var rows []model.AdminLevelLabel
	if err := s.db.WithContext(ctx).
		Where("country_code = ?", country).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("level labels: %w", err)
	}
	out := make(map[int]string, len(rows))
	for _, r := range rows {
		out[r.Level] = r.Label
	}
	return out, nil
}

// resolveRegion finds candidate parents for a region name. It returns a slice
// because a region name is not guaranteed unique either.
func (s *Service) resolveRegion(ctx context.Context, country, region string) ([]uint, error) {
	key := normalize.Key(region)
	var rows []model.Location
	if err := s.db.WithContext(ctx).
		Where("country_code = ? AND name_key = ?", country, key).
		Limit(20).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("resolve region: %w", err)
	}
	if len(rows) == 0 {
		// Fall back to fuzzy so a misspelled province does not hard-fail the
		// whole request.
		cands, err := s.candidates(ctx, country, key, nil)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			if normalize.Similarity(key, c.NameKey) >= AcceptThreshold {
				rows = append(rows, c)
			}
		}
	}
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

func (s *Service) lookupExact(ctx context.Context, country, key string, parents []uint) (*model.Location, error) {
	q := s.db.WithContext(ctx).Where("country_code = ? AND name_key = ?", country, key)
	if len(parents) > 0 {
		q = q.Where("parent_id IN ?", parents)
	}
	var rows []model.Location
	if err := q.Order("population DESC").Limit(5).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("exact lookup: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// Several places can share a name. Without a region filter the largest is
	// the best guess - a caller asking for "Merkez" with no province almost
	// certainly does not mean a village of 200 people.
	return &rows[0], nil
}

// candidates narrows the fuzzy search with an indexed prefix scan. Rows whose
// length differs wildly from the input are dropped before scoring, since edit
// distance cannot bring them above the threshold anyway.
func (s *Service) candidates(ctx context.Context, country, key string, parents []uint) ([]model.Location, error) {
	runes := []rune(key)
	p := prefixLen
	if len(runes) < p {
		p = len(runes)
	}
	prefix := string(runes[:p])

	q := s.db.WithContext(ctx).
		Where("country_code = ? AND name_key LIKE ?", country, prefix+"%")
	if len(parents) > 0 {
		q = q.Where("parent_id IN ?", parents)
	}

	var rows []model.Location
	if err := q.Order("population DESC").Limit(maxCandidates).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("candidate scan: %w", err)
	}

	// A first-character typo defeats the prefix scan entirely. Retry once on a
	// shorter prefix rather than returning nothing.
	if len(rows) == 0 && len(runes) >= 2 {
		q2 := s.db.WithContext(ctx).
			Where("country_code = ? AND name_key LIKE ?", country, string(runes[:1])+"%")
		if len(parents) > 0 {
			q2 = q2.Where("parent_id IN ?", parents)
		}
		if err := q2.Order("population DESC").Limit(maxCandidates).Find(&rows).Error; err != nil {
			return nil, fmt.Errorf("candidate rescan: %w", err)
		}
	}
	return rows, nil
}

func (s *Service) ancestry(ctx context.Context, loc *model.Location, labels map[int]string) ([]HierarchyNode, error) {
	var out []HierarchyNode
	cur := loc
	// Depth is bounded by the deepest hierarchy GeoNames models (admin1..4),
	// but guard anyway: a bad import could introduce a cycle.
	for i := 0; i < 8 && cur != nil && cur.ParentID != nil; i++ {
		var parent model.Location
		if err := s.db.WithContext(ctx).First(&parent, *cur.ParentID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				break
			}
			return nil, fmt.Errorf("ancestry: %w", err)
		}
		out = append(out, HierarchyNode{
			Level: parent.Level, LevelLabel: labels[parent.Level], Name: parent.Name,
		})
		cur = &parent
	}
	// Return outermost-first (country side down to the match).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func scoreAll(rows []model.Location, key string, labels map[int]string) []Match {
	out := make([]Match, 0, len(rows))
	for _, r := range rows {
		sc := normalize.Similarity(key, r.NameKey)
		if sc < SuggestThreshold {
			continue
		}
		out = append(out, Match{
			ID: r.ID, Name: r.Name, Level: r.Level,
			LevelLabel: labels[r.Level], Score: round2(sc),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

func topN(in []Match, n int, min float64) []Match {
	var out []Match
	for _, m := range in {
		if m.Score < min {
			break // input is already sorted descending
		}
		out = append(out, m)
		if len(out) == n {
			break
		}
	}
	return out
}

func findByID(rows []model.Location, id uint) *model.Location {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
