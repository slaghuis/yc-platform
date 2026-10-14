// Placeholder entry point for yc-pricing.
// See docs/services/yc-pricing.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-pricing placeholder\",\"version\":%q}\n", version)
}