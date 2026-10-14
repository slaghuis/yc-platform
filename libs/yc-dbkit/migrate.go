package dbkit

import (
	stderrors "errors"
	"fmt"
	"io/fs"
	"net/url"

	"github.com/golang-migrate/migrate/v4"
	// pgx5:// database driver for golang-migrate.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Migrate applies every pending up migration in migrations (a directory of
// NNN_name.up.sql / NNN_name.down.sql files, normally embedded by the
// service's migrations package) to the database at dbURL.
func Migrate(dbURL string, migrations fs.FS) error {
	src, err := iofs.New(migrations, ".")
	if err != nil {
		return fmt.Errorf("dbkit: migrations source: %w", err)
	}
	driverURL, err := pgx5URL(dbURL)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, driverURL)
	if err != nil {
		return fmt.Errorf("dbkit: migrate init: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !stderrors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("dbkit: migrate up: %w", err)
	}
	return nil
}

// pgx5URL rewrites a postgres:// URL to the pgx5:// scheme golang-migrate's
// pgx driver registers under.
func pgx5URL(dbURL string) (string, error) {
	u, err := url.Parse(dbURL)
	if err != nil {
		return "", fmt.Errorf("dbkit: parse url: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		u.Scheme = "pgx5"
	case "pgx5":
	default:
		return "", fmt.Errorf("dbkit: unsupported url scheme %q", u.Scheme)
	}
	return u.String(), nil
}
