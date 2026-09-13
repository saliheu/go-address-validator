// Package geonames imports the GeoNames open dataset (CC BY 4.0) into the
// locations tree.
//
// Files this understands, all tab-separated with no header:
//
//	admin1CodesASCII.txt   TR.35        Izmir            Izmir        311046
//	admin2Codes.txt        TR.35.35010  Konak            Konak       6941405
//	<CC>.txt               19 columns, one row per populated place
//	<CC>.txt (postal zip)  12 columns, one row per postal code
//
// Rows are inserted in batches. Row-at-a-time inserts are the obvious
// implementation and are roughly two orders of magnitude slower here: TR.txt
// alone is ~50k rows, and allCountries is millions.
package geonames

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/saliheu/go-address-validator/internal/model"
	"github.com/saliheu/go-address-validator/internal/normalize"
)

const batchSize = 1000

type Importer struct {
	db *gorm.DB
}

func NewImporter(db *gorm.DB) *Importer { return &Importer{db: db} }

// tsvReader configures a CSV reader for GeoNames files. Quotes must be
// disabled: place names legitimately contain a double quote character, and the
// files are not quoted at all, so the default parser fails on them.
func tsvReader(r io.Reader) *csv.Reader {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.Comma = '\t'
	cr.LazyQuotes = true
	cr.FieldsPerRecord = -1 // row width varies between the files
	cr.ReuseRecord = true
	return cr
}

// ImportAdmin1 loads first-level subdivisions, keyed "CC.CODE".
// Returns a map of that key to the inserted row ID, for use by ImportAdmin2.
func (im *Importer) ImportAdmin1(path, countryFilter string) (map[string]uint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	ids := map[string]uint{}
	var batch []model.Location
	var keys []string

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := im.db.Create(&batch).Error; err != nil {
			return err
		}
		for i := range batch {
			ids[keys[i]] = batch[i].ID
		}
		batch, keys = batch[:0], keys[:0]
		return nil
	}

	cr := tsvReader(f)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read admin1: %w", err)
		}
		if len(rec) < 4 {
			continue
		}
		code := rec[0] // "TR.35"
		cc, _, ok := strings.Cut(code, ".")
		if !ok || (countryFilter != "" && cc != countryFilter) {
			continue
		}
		name := rec[1]
		gid, _ := strconv.ParseInt(rec[3], 10, 64)

		batch = append(batch, model.Location{
			CountryCode: cc, Level: 1, Name: name,
			NameKey: normalize.Key(name), GeonameID: gid,
		})
		keys = append(keys, code)

		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	log.Printf("geonames: imported %d admin1 rows", len(ids))
	return ids, nil
}

// ImportAdmin2 loads second-level subdivisions, keyed "CC.A1.A2", linking each
// to its admin1 parent.
func (im *Importer) ImportAdmin2(path, countryFilter string, admin1IDs map[string]uint) (map[string]uint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	ids := map[string]uint{}
	var batch []model.Location
	var keys []string
	orphans := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := im.db.Create(&batch).Error; err != nil {
			return err
		}
		for i := range batch {
			ids[keys[i]] = batch[i].ID
		}
		batch, keys = batch[:0], keys[:0]
		return nil
	}

	cr := tsvReader(f)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read admin2: %w", err)
		}
		if len(rec) < 4 {
			continue
		}
		code := rec[0] // "TR.35.35010"
		parts := strings.Split(code, ".")
		if len(parts) < 3 {
			continue
		}
		cc := parts[0]
		if countryFilter != "" && cc != countryFilter {
			continue
		}
		parentID, ok := admin1IDs[cc+"."+parts[1]]
		if !ok {
			// The datasets are not perfectly consistent; an admin2 can
			// reference an admin1 that is absent. Skip rather than abort, but
			// count it so a broken import is visible in the log.
			orphans++
			continue
		}
		name := rec[1]
		gid, _ := strconv.ParseInt(rec[3], 10, 64)

		pid := parentID
		batch = append(batch, model.Location{
			CountryCode: cc, Level: 2, Name: name,
			NameKey: normalize.Key(name), ParentID: &pid, GeonameID: gid,
		})
		keys = append(keys, code)

		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	log.Printf("geonames: imported %d admin2 rows (%d orphaned)", len(ids), orphans)
	return ids, nil
}

// ImportPlaces loads populated places (feature class P) from a per-country
// dump, attaching each to the deepest admin parent that exists.
func (im *Importer) ImportPlaces(path string, admin1IDs, admin2IDs map[string]uint) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var batch []model.Location
	total := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := im.db.Create(&batch).Error; err != nil {
			return err
		}
		total += len(batch)
		batch = batch[:0]
		return nil
	}

	cr := tsvReader(f)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, fmt.Errorf("read places: %w", err)
		}
		if len(rec) < 15 {
			continue
		}
		// Column 6 is the feature class. "P" is populated places; the file
		// also holds mountains, streams and administrative polygons, none of
		// which belong in an address validator.
		if rec[6] != "P" {
			continue
		}

		gid, _ := strconv.ParseInt(rec[0], 10, 64)
		name := rec[1]
		cc := rec[8]
		a1, a2 := rec[10], rec[11]
		pop, _ := strconv.ParseInt(rec[14], 10, 64)

		var parentID *uint
		if id, ok := admin2IDs[cc+"."+a1+"."+a2]; ok {
			parentID = &id
		} else if id, ok := admin1IDs[cc+"."+a1]; ok {
			parentID = &id
		}

		batch = append(batch, model.Location{
			CountryCode: cc, Level: 3, Name: name,
			NameKey: normalize.Key(name), ParentID: parentID,
			GeonameID: gid, Population: pop,
		})

		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}
	log.Printf("geonames: imported %d populated places", total)
	return total, nil
}

// ImportPostalCodes loads the postal-code dataset, which is a separate download
// with a different 12-column layout.
func (im *Importer) ImportPostalCodes(path, countryFilter string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var batch []model.PostalCode
	total := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := im.db.Create(&batch).Error; err != nil {
			return err
		}
		total += len(batch)
		batch = batch[:0]
		return nil
	}

	cr := tsvReader(f)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, fmt.Errorf("read postal: %w", err)
		}
		if len(rec) < 7 {
			continue
		}
		cc := rec[0]
		if countryFilter != "" && cc != countryFilter {
			continue
		}
		code, place, a1, a2 := rec[1], rec[2], rec[3], rec[5]

		batch = append(batch, model.PostalCode{
			CountryCode: cc,
			Code:        code,
			CodeKey:     strings.ReplaceAll(normalize.Key(code), " ", ""),
			PlaceName:   place,
			PlaceKey:    normalize.Key(place),
			Admin1Name:  a1,
			Admin1Key:   normalize.Key(a1),
			Admin2Name:  a2,
			Admin2Key:   normalize.Key(a2),
		})

		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}
	log.Printf("geonames: imported %d postal codes", total)
	return total, nil
}

// UpsertCountry writes a country and the labels for its admin levels.
func (im *Importer) UpsertCountry(c model.Country, labels []model.AdminLevelLabel) error {
	if err := im.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&c).Error; err != nil {
		return fmt.Errorf("upsert country %s: %w", c.Code, err)
	}
	for _, l := range labels {
		if err := im.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&l).Error; err != nil {
			return fmt.Errorf("upsert label %s/%d: %w", l.CountryCode, l.Level, err)
		}
	}
	return nil
}
