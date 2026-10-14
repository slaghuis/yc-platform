// Placeholder entry point for yc-preference.
// See docs/services/yc-preference.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-preference placeholder\",\"version\":%q}\n", version)
}