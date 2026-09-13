package geonames

import (
	"log"

	"gorm.io/gorm"

	"github.com/saliheu/go-address-validator/internal/model"
	"github.com/saliheu/go-address-validator/internal/normalize"
)

// Seed loads a small, hand-checked dataset so the service is useful the moment
// `docker compose up` finishes. The full GeoNames import is a ~350 MB download;
// nobody evaluating this repo should have to do that before seeing it work.
//
// The countries here are chosen to exercise the variable-depth design rather
// than for coverage:
//
//	TR  il -> ilce -> yerlesim   (3 levels)
//	US  state -> county -> city  (3 levels, different names)
//	DE  Bundesland -> Stadt      (2 levels)
//	MC  no subdivision at all    (1 level)
//
// A schema with fixed province/district/city columns cannot hold all four.
func Seed(db *gorm.DB) error {
	var existing int64
	if err := db.Model(&model.Location{}).Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		log.Printf("seed: %d locations already present, skipping", existing)
		return nil
	}

	im := NewImporter(db)

	countries := []struct {
		c      model.Country
		labels []model.AdminLevelLabel
	}{
		{
			model.Country{Code: "TR", Name: "Türkiye", MaxLevel: 3},
			[]model.AdminLevelLabel{
				{CountryCode: "TR", Level: 1, Label: "il", LabelEn: "province"},
				{CountryCode: "TR", Level: 2, Label: "ilçe", LabelEn: "district"},
				{CountryCode: "TR", Level: 3, Label: "yerleşim", LabelEn: "settlement"},
			},
		},
		{
			model.Country{Code: "US", Name: "United States", MaxLevel: 3},
			[]model.AdminLevelLabel{
				{CountryCode: "US", Level: 1, Label: "state", LabelEn: "state"},
				{CountryCode: "US", Level: 2, Label: "county", LabelEn: "county"},
				{CountryCode: "US", Level: 3, Label: "city", LabelEn: "city"},
			},
		},
		{
			model.Country{Code: "DE", Name: "Deutschland", MaxLevel: 2},
			[]model.AdminLevelLabel{
				{CountryCode: "DE", Level: 1, Label: "Bundesland", LabelEn: "state"},
				{CountryCode: "DE", Level: 2, Label: "Stadt", LabelEn: "city"},
			},
		},
		{
			model.Country{Code: "MC", Name: "Monaco", MaxLevel: 1},
			[]model.AdminLevelLabel{
				{CountryCode: "MC", Level: 1, Label: "commune", LabelEn: "commune"},
			},
		},
	}
	for _, entry := range countries {
		if err := im.UpsertCountry(entry.c, entry.labels); err != nil {
			return err
		}
	}

	// tree describes level-1 regions and their children.
	type child struct {
		name string
		pop  int64
	}
	tree := []struct {
		cc       string
		region   string
		children []child
	}{
		// Istanbul carries the diacritic showcase: "Şişli" must match input
		// typed as "Sisli", and "Üsküdar" as "Uskudar".
		{"TR", "İstanbul", []child{
			{"Şişli", 274000}, {"Kadıköy", 467000}, {"Beşiktaş", 176000},
			{"Üsküdar", 524000}, {"Beyoğlu", 226000}, {"Bakırköy", 226000},
		}},
		{"TR", "İzmir", []child{
			{"Konak", 337000}, {"Bornova", 449000}, {"Karşıyaka", 343000},
			{"Buca", 519000}, {"Urla", 65000}, {"Çeşme", 45000},
		}},
		{"TR", "Manisa", []child{
			{"Yunusemre", 260000}, {"Şehzadeler", 170000}, {"Turgutlu", 165000},
			{"Akhisar", 170000}, {"Salihli", 155000}, {"Soma", 105000},
		}},
		{"TR", "Eskişehir", []child{
			{"Odunpazarı", 400000}, {"Tepebaşı", 350000}, {"Sivrihisar", 20000},
		}},
		{"TR", "Konya", []child{
			{"Selçuklu", 660000}, {"Meram", 340000}, {"Karatay", 300000},
			{"Ereğli", 145000},
		}},
		{"TR", "Isparta", []child{
			{"Merkez", 260000}, {"Eğirdir", 32000}, {"Yalvaç", 45000},
		}},
		{"TR", "Ankara", []child{
			{"Çankaya", 950000}, {"Keçiören", 940000}, {"Yenimahalle", 700000},
			{"Sincan", 550000},
		}},
		{"TR", "Bursa", []child{
			{"Osmangazi", 880000}, {"Nilüfer", 500000}, {"Yıldırım", 660000},
		}},
		// Long name that callers routinely truncate to "Afyon": exercises the
		// prefix rule in normalize.Similarity.
		{"TR", "Afyonkarahisar", []child{
			{"Merkez", 190000}, {"Sandıklı", 55000},
		}},
		{"TR", "Kocaeli", []child{
			{"İzmit", 370000}, {"Gebze", 400000},
		}},

		{"US", "California", []child{
			{"Los Angeles County", 9800000}, {"San Francisco County", 870000},
		}},
		{"US", "New York", []child{
			{"New York County", 1600000}, {"Kings County", 2600000},
		}},

		{"DE", "Hessen", []child{
			{"Frankfurt am Main", 760000}, {"Wiesbaden", 280000},
		}},
		{"DE", "Bayern", []child{
			{"München", 1500000}, {"Nürnberg", 520000},
		}},

		// Monaco has no subdivision: the commune sits directly under the
		// country with no parent.
		{"MC", "Monaco", nil},
	}

	for _, node := range tree {
		region := model.Location{
			CountryCode: node.cc, Level: 1, Name: node.region,
			NameKey: normalize.Key(node.region),
		}
		if err := db.Create(&region).Error; err != nil {
			return err
		}
		for _, ch := range node.children {
			pid := region.ID
			row := model.Location{
				CountryCode: node.cc, Level: 2, Name: ch.name,
				NameKey: normalize.Key(ch.name), ParentID: &pid, Population: ch.pop,
			}
			if err := db.Create(&row).Error; err != nil {
				return err
			}
		}
	}

	// A handful of level-3 cities under US counties, to show a three-deep path.
	var laCounty, nyCounty model.Location
	db.Where("country_code = ? AND name_key = ?", "US", normalize.Key("Los Angeles County")).First(&laCounty)
	db.Where("country_code = ? AND name_key = ?", "US", normalize.Key("New York County")).First(&nyCounty)
	for _, c := range []struct {
		name   string
		parent *model.Location
		pop    int64
	}{
		{"Los Angeles", &laCounty, 3900000},
		{"Pasadena", &laCounty, 138000},
		{"New York City", &nyCounty, 8300000},
	} {
		if c.parent.ID == 0 {
			continue
		}
		pid := c.parent.ID
		row := model.Location{
			CountryCode: "US", Level: 3, Name: c.name,
			NameKey: normalize.Key(c.name), ParentID: &pid, Population: c.pop,
		}
		if err := db.Create(&row).Error; err != nil {
			return err
		}
	}

	postal := []model.PostalCode{
		{CountryCode: "TR", Code: "35250", PlaceName: "Konak", Admin1Name: "İzmir"},
		{CountryCode: "TR", Code: "35030", PlaceName: "Bornova", Admin1Name: "İzmir"},
		{CountryCode: "TR", Code: "45010", PlaceName: "Yunusemre", Admin1Name: "Manisa"},
		{CountryCode: "TR", Code: "45400", PlaceName: "Turgutlu", Admin1Name: "Manisa"},
		{CountryCode: "TR", Code: "45200", PlaceName: "Akhisar", Admin1Name: "Manisa"},
		{CountryCode: "TR", Code: "26010", PlaceName: "Odunpazarı", Admin1Name: "Eskişehir"},
		{CountryCode: "TR", Code: "42060", PlaceName: "Selçuklu", Admin1Name: "Konya"},
		{CountryCode: "TR", Code: "32100", PlaceName: "Merkez", Admin1Name: "Isparta"},
		{CountryCode: "TR", Code: "06510", PlaceName: "Çankaya", Admin1Name: "Ankara"},
		{CountryCode: "TR", Code: "16110", PlaceName: "Nilüfer", Admin1Name: "Bursa"},
		{CountryCode: "US", Code: "90001", PlaceName: "Los Angeles", Admin1Name: "California"},
		{CountryCode: "US", Code: "10001", PlaceName: "New York City", Admin1Name: "New York"},
		{CountryCode: "DE", Code: "60311", PlaceName: "Frankfurt am Main", Admin1Name: "Hessen"},
		{CountryCode: "MC", Code: "98000", PlaceName: "Monaco", Admin1Name: ""},
	}
	for i := range postal {
		postal[i].CodeKey = normalize.Key(postal[i].Code)
		postal[i].PlaceKey = normalize.Key(postal[i].PlaceName)
		postal[i].Admin1Key = normalize.Key(postal[i].Admin1Name)
	}
	if err := db.Create(&postal).Error; err != nil {
		return err
	}

	var locCount int64
	db.Model(&model.Location{}).Count(&locCount)
	log.Printf("seed: %d locations, %d postal codes, %d countries",
		locCount, len(postal), len(countries))
	return nil
}
