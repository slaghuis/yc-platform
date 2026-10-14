// Placeholder entry point for <name>.
// See docs/services/<name>.md for the full requirement spec.
package main

import (
    "fmt"
    "os"
)

var version = "dev"

func main() {
    fmt.Fprintf(os.Stdout, "{\"msg\":\"<name> placeholder\",\"version\":%q}\n", version)
}