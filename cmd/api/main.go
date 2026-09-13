// Command api runs the address validation service.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/saliheu/go-address-validator/internal/auth"
	"github.com/saliheu/go-address-validator/internal/cache"
	"github.com/saliheu/go-address-validator/internal/config"
	"github.com/saliheu/go-address-validator/internal/geonames"
	"github.com/saliheu/go-address-validator/internal/httpapi"
	"github.com/saliheu/go-address-validator/internal/store"
	"github.com/saliheu/go-address-validator/internal/validate"
)

func main() {
	var (
		importDir = flag.String("import", "",
			"directory holding GeoNames files; imports and exits instead of serving")
		importCountry = flag.String("country", "",
			"restrict -import to this ISO-3166-1 alpha-2 code")
		skipSeed = flag.Bool("no-seed", false,
			"skip the built-in demo dataset")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	db, err := store.Open(store.Config{
		Host: cfg.DB.Host, Port: cfg.DB.Port, User: cfg.DB.User,
		Password: cfg.DB.Password, DBName: cfg.DB.Name,
		SSLMode: cfg.DB.SSLMode, Debug: cfg.Debug(),
	})
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("database ready")

	if *importDir != "" {
		if err := runImport(db, *importDir, *importCountry); err != nil {
			log.Fatalf("import: %v", err)
		}
		return
	}

	if !*skipSeed {
		if err := geonames.Seed(db); err != nil {
			log.Fatalf("seed: %v", err)
		}
	}

	c := cache.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.TTL)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := c.Ping(ctx); err != nil {
		// Not fatal by design: the service is meant to survive a cache outage.
		log.Printf("redis unreachable (%v) - serving without cache", err)
	} else {
		log.Println("cache ready")
	}
	cancel()

	gin.SetMode(cfg.GinMode)
	api := httpapi.New(
		db,
		validate.New(db),
		auth.New(db, cfg.Auth.AccessSecret, cfg.Auth.RefreshSecret,
			cfg.Auth.AccessTTL, cfg.Auth.RefreshTTL),
		c,
	)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Drain in-flight requests before exiting, so a deploy does not return
	// errors to callers that were mid-request.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("forced shutdown: %v", err)
	}
	log.Println("stopped")
}

// runImport loads whichever GeoNames files are present in dir. Missing files
// are skipped rather than treated as errors: a caller who only wants postal
// codes should not have to download the full place dump.
func runImport(db *gorm.DB, dir, country string) error {
	im := geonames.NewImporter(db)
	join := func(name string) string { return filepath.Join(dir, name) }

	exists := func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && !info.IsDir()
	}

	admin1IDs := map[string]uint{}
	admin2IDs := map[string]uint{}

	if p := join("admin1CodesASCII.txt"); exists(p) {
		ids, err := im.ImportAdmin1(p, country)
		if err != nil {
			return err
		}
		admin1IDs = ids
	} else {
		log.Printf("skip: %s not found", p)
	}

	if p := join("admin2Codes.txt"); exists(p) {
		ids, err := im.ImportAdmin2(p, country, admin1IDs)
		if err != nil {
			return err
		}
		admin2IDs = ids
	} else {
		log.Printf("skip: %s not found", p)
	}

	if country != "" {
		if p := join(country + ".txt"); exists(p) {
			if _, err := im.ImportPlaces(p, admin1IDs, admin2IDs); err != nil {
				return err
			}
		} else {
			log.Printf("skip: %s not found", p)
		}
		// The postal dataset ships a same-named file, so it must live in a
		// separate subdirectory to avoid colliding with the place dump.
		if p := join("postal/" + country + ".txt"); exists(p) {
			if _, err := im.ImportPostalCodes(p, country); err != nil {
				return err
			}
		} else {
			log.Printf("skip: %s not found", p)
		}
	} else {
		log.Println("no -country given; skipping place and postal import")
	}

	log.Println("import complete")
	return nil
}
