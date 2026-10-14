// Placeholder entry point for yc-audit.
// See docs/services/yc-audit.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-audit placeholder\",\"version\":%q}\n", version)
}