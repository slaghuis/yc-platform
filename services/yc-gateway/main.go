// Placeholder entry point for yc-gateway.
// See docs/services/yc-gateway.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-gateway placeholder\",\"version\":%q}\n", version)
}