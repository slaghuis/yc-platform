// Placeholder entry point for yc-monitor.
// See docs/services/yc-monitor.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-monitor placeholder\",\"version\":%q}\n", version)
}