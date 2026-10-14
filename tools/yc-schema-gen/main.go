// yc-schema-gen reads the JSON Schemas in schemas/yc-event-schemas and
// writes libs/yc-events: Go types for every event payload and common
// fragment (types_gen.go), plus a copy of the schemas for the runtime
// validator to embed. Run it from the repo root via `task gen:events`.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func main() {
	schemaDir := flag.String("schemas", "schemas/yc-event-schemas", "schema source directory")
	outDir := flag.String("out", "libs/yc-events", "yc-events module directory")
	flag.Parse()

	if err := run(*schemaDir, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "yc-schema-gen:", err)
		os.Exit(1)
	}
}

func run(schemaDir, outDir string) error {
	files, err := loadSchemas(schemaDir)
	if err != nil {
		return err
	}
	src, err := generate(files)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "types_gen.go"), src, 0o644); err != nil { //nolint:gosec // generated source is world-readable
		return err
	}
	return copySchemas(files, filepath.Join(outDir, "schemas"))
}

func copySchemas(files []schemaFile, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dst, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(path), fs.ModePerm); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.raw, 0o644); err != nil { //nolint:gosec // schemas are public
			return err
		}
	}
	return nil
}
