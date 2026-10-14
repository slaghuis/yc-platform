// Placeholder entry point for yc-payment.
// See docs/services/yc-payment.md for the full requirement spec.
package main

import (
    "fmt"
)

var version = "dev"

func main() {
    fmt.Printf("{\"msg\":\"yc-payment placeholder\",\"version\":%q}\n", version)
}