// yc-migrate wraps golang-migrate to apply migrations to one or all services'
// databases.
//
// Placeholder: full implementation lands with Phase 1.
package main

import (
    "fmt"
    "os"
)

func main() {
    fmt.Fprintln(os.Stdout, "yc-migrate: placeholder (not yet implemented)")
    fmt.Fprintln(os.Stdout, "  usage: yc-migrate [up|down|new] [--service=<name>|--all] [--name=<migration>]")
    if len(os.Args) < 2 {
        os.Exit(0)
    }
}