package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFingerprint(t *testing.T) {
	a := Item{SKUID: "b", Quantity: 2}
	b := Item{SKUID: "a", Quantity: 1}
	assert.Equal(t, "a:1|b:2", Fingerprint([]Item{a, b}))
	assert.Equal(t, Fingerprint([]Item{a, b}), Fingerprint([]Item{b, a}))
	assert.NotEqual(t, Fingerprint([]Item{a, b}), Fingerprint([]Item{a, {SKUID: "b", Quantity: 3}}))
	assert.Empty(t, Fingerprint(nil))
}
