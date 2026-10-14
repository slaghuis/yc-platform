// Placeholder entry point for yc-product.
// See docs/services/yc-product.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-product placeholder\",\"version\":%q}\n", version)
}