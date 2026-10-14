// Placeholder entry point for yc-image.
// See docs/services/yc-image.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-image placeholder\",\"version\":%q}\n", version)
}