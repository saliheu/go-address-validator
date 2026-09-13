// Package model holds the persisted schema.
//
// The design problem this service exists to solve: administrative hierarchies
// are not the same shape in every country. Turkey has il -> ilce. The US has
// state -> county -> city. Monaco has no subdivision at all. Modelling one
// table per country does not scale, and a fixed province/district/city triple
// forces every country into a shape that fits almost none of them.
//
// The approach here is a single self-referencing locations table with a numeric
// level, plus a per-country lookup that says what each level is *called*. The
// storage shape stays constant; only the labels and the depth vary.
package model

import "time"

// Country is an ISO-3166-1 alpha-2 entry.
type Country struct {
	Code      string    `gorm:"primaryKey;size:2"  json:"code"`
	Name      string    `gorm:"size:128;not null"  json:"name"`
	MaxLevel  int       `gorm:"not null;default:0" json:"max_level"` // deepest admin level in use
	CreatedAt time.Time `json:"-"`
}

// AdminLevelLabel names one administrative level for one country.
//
//	("TR", 1, "il")      ("TR", 2, "ilce")
//	("US", 1, "state")   ("US", 2, "county")
//
// This is what lets the API answer in the caller's own vocabulary instead of
// leaking "admin1"/"admin2" to clients.
type AdminLevelLabel struct {
	CountryCode string `gorm:"primaryKey;size:2" json:"country_code"`
	Level       int    `gorm:"primaryKey"        json:"level"`
	Label       string `gorm:"size:64;not null"  json:"label"`
	LabelEn     string `gorm:"size:64"           json:"label_en"`
}

// Location is one node of the hierarchy: a country subdivision or a populated
// place. Level 1..N mirrors GeoNames admin1..adminN; the deepest level holds
// cities and towns.
type Location struct {
	ID          uint   `gorm:"primaryKey"            json:"id"`
	CountryCode string `gorm:"size:2;not null;index" json:"country_code"`
	Level       int    `gorm:"not null"              json:"level"`
	Name        string `gorm:"size:200;not null"     json:"name"`
	// NameKey is the normalized form produced by normalize.Key. Every lookup
	// goes through this column, never through Name.
	NameKey    string `gorm:"size:200;not null" json:"-"`
	ParentID   *uint  `gorm:"index"             json:"parent_id,omitempty"`
	GeonameID  int64  `gorm:"index"             json:"geoname_id,omitempty"`
	Population int64  `gorm:"default:0"         json:"population,omitempty"`

	Parent *Location `gorm:"foreignKey:ParentID" json:"-"`
}

// PostalCode comes from a separate GeoNames dataset and does not always line up
// cleanly with the locations tree: one code can cover several places, and one
// place can hold many codes. It is therefore stored flat, with denormalized
// admin keys for filtering rather than a hard foreign key.
type PostalCode struct {
	ID          uint   `gorm:"primaryKey"            json:"id"`
	CountryCode string `gorm:"size:2;not null;index" json:"country_code"`
	Code        string `gorm:"size:20;not null"      json:"code"`
	CodeKey     string `gorm:"size:20;not null"      json:"-"`
	PlaceName   string `gorm:"size:200"              json:"place_name"`
	PlaceKey    string `gorm:"size:200"              json:"-"`
	Admin1Name  string `gorm:"size:200"              json:"admin1_name,omitempty"`
	Admin1Key   string `gorm:"size:200"              json:"-"`
	Admin2Name  string `gorm:"size:200"              json:"admin2_name,omitempty"`
	Admin2Key   string `gorm:"size:200"              json:"-"`
}

// User is an API consumer. Passwords are stored as bcrypt hashes.
type User struct {
	ID           uint      `gorm:"primaryKey"                    json:"id"`
	Email        string    `gorm:"size:255;uniqueIndex;not null" json:"email"`
	PasswordHash string    `gorm:"size:255;not null"             json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// RefreshToken implements rotation: every refresh issues a new token and marks
// the old one used. A token presented twice means it leaked, so the whole
// family is revoked rather than only the replay being rejected.
type RefreshToken struct {
	ID        uint       `gorm:"primaryKey"                   json:"id"`
	UserID    uint       `gorm:"index;not null"               json:"user_id"`
	TokenHash string     `gorm:"size:64;uniqueIndex;not null" json:"-"` // sha256 hex, never the token
	FamilyID  string     `gorm:"size:36;index;not null"       json:"-"`
	ExpiresAt time.Time  `gorm:"not null"                     json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
