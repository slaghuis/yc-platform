// Placeholder entry point for yc-orders.
// See docs/services/yc-orders.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-orders placeholder\",\"version\":%q}\n", version)
}