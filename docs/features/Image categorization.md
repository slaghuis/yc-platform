# Categorise images

## Overview
**Status:** Cancelled
**GitHub Issue:** #18 (closed, not planned)
**Repository:** yc-platform

## Decision
No image categories in yc-image. A product already marks one attached image with `is_primary` in yc-product. That flag is how a product list and a product detail page choose the image to show. A second flag or category in yc-image would duplicate it.

Issue #17 is not part of this decision.
