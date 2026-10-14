// Placeholder entry point for yc-cart.
// See docs/services/yc-cart.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-cart placeholder\",\"version\":%q}\n", version)
}