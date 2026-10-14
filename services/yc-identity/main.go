// Placeholder entry point for yc-identity.
// See docs/services/yc-identity.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-identity placeholder\",\"version\":%q}\n", version)
}