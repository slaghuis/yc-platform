// Placeholder entry point for yc-shipping.
// See docs/services/yc-shipping.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-shipping placeholder\",\"version\":%q}\n", version)
}