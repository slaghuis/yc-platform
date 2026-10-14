// yc-schema-gen reads JSON Schemas from schemas/yc-event-schemas and
// generates Go structs into libs/yc-events/generated.
//
// Placeholder: full implementation lands with Phase 1 (walking skeleton).
package main

import (
    "fmt"
    "os"
)

func main() {
    fmt.Fprintln(os.Stdout, "yc-schema-gen: placeholder (not yet implemented)")
    fmt.Fprintln(os.Stdout, "  will generate libs/yc-events/generated/*.go from schemas/yc-event-schemas/**/*.json")
}