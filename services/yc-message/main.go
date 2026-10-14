// Placeholder entry point for yc-message.
// See docs/services/yc-message.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-message placeholder\",\"version\":%q}\n", version)
}