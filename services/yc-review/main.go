// Placeholder entry point for yc-review.
// See docs/services/yc-review.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-review placeholder\",\"version\":%q}\n", version)
}