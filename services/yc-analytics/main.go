// Placeholder entry point for yc-analytics.
// See docs/services/yc-analytics.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-analytics placeholder\",\"version\":%q}\n", version)
}