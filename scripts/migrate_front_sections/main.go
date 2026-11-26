// Command migrate_front_sections converts the retired front page settings in a sorus.db.
// It is intended to be run once before upgrading an existing installation.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"sorus/internal/frontmigration"

	_ "modernc.org/sqlite"
)

type section = frontmigration.Section

func convert(values map[string]string) ([]section, error) { return frontmigration.Convert(values) }

func run(ctx context.Context, path string, dryRun bool) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	return frontmigration.Migrate(ctx, db, dryRun, func(message string) { fmt.Println(message) })
}

func main() {
	dbPath := flag.String("db", "sorus.db", "path to sorus.db")
	dry := flag.Bool("dry-run", false, "show planned changes without writing")
	flag.Parse()
	if err := run(context.Background(), *dbPath, *dry); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
