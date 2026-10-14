// Placeholder entry point for yc-checkout.
// See docs/services/yc-checkout.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-checkout placeholder\",\"version\":%q}\n", version)
}