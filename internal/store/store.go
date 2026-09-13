// Package store owns the database connection and schema migration.
package store

import (
	"fmt"
	"log"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/saliheu/go-address-validator/internal/model"
)

type Config struct {
	Host, Port, User, Password, DBName, SSLMode string
	Debug                                       bool
}

// Open connects and retries: under docker compose the API container is
// routinely up before Postgres finishes its first-run initialisation.
func Open(cfg Config) (*gorm.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName, cfg.SSLMode,
	)

	logLevel := logger.Warn
	if cfg.Debug {
		logLevel = logger.Info
	}

	var db *gorm.DB
	var err error
	for attempt := 1; attempt <= 10; attempt++ {
		db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logLevel),
		})
		if err == nil {
			break
		}
		wait := time.Duration(attempt) * time.Second
		log.Printf("db not ready (attempt %d/10): %v - retrying in %s", attempt, err, wait)
		time.Sleep(wait)
	}
	if err != nil {
		return nil, fmt.Errorf("connect after 10 attempts: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	return db, nil
}

// Migrate creates the schema and the indexes the lookup path depends on.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&model.Country{},
		&model.AdminLevelLabel{},
		&model.Location{},
		&model.PostalCode{},
		&model.User{},
		&model.RefreshToken{},
	); err != nil {
		return fmt.Errorf("automigrate: %w", err)
	}

	// Every lookup filters on country first, then level, then the normalized
	// key - so the composite index has to be in that order to be usable. A
	// plain index on name_key alone would force a scan across all countries.
	statements := []string{
		"CREATE INDEX IF NOT EXISTS idx_loc_country_level_key " +
			"ON locations (country_code, level, name_key)",

		// Parent-scoped child lookup: "districts of this province".
		"CREATE INDEX IF NOT EXISTS idx_loc_parent_key " +
			"ON locations (parent_id, name_key)",

		// Postal lookup is always country + code.
		"CREATE INDEX IF NOT EXISTS idx_postal_country_code " +
			"ON postal_codes (country_code, code_key)",

		// Reverse direction: "which postcodes belong to this place".
		"CREATE INDEX IF NOT EXISTS idx_postal_country_place " +
			"ON postal_codes (country_code, place_key)",

		// Prefix scans (name_key LIKE 'anka%') cannot use a default btree
		// index under a non-C collation. text_pattern_ops fixes that, and is
		// what keeps the fuzzy candidate query off a sequential scan.
		"CREATE INDEX IF NOT EXISTS idx_loc_key_prefix " +
			"ON locations (name_key text_pattern_ops)",
	}
	for _, stmt := range statements {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("index: %w", err)
		}
	}
	return nil
}
