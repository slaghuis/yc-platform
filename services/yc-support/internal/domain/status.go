// Package domain holds yc-support's rules: statuses and their transitions,
// topics, contact validation and thread references. It does no I/O.
package domain

import (
	"fmt"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Status is where a request is in its life (docs/features/Customer support
// contact threads.md, status machine).
type Status string

// Request statuses. Closed is terminal: there is no reopen.
const (
	StatusNew          Status = "new"           // no shop message yet; the shop acts next
	StatusOpenCustomer Status = "open_customer" // the shop answered; the requester acts next
	StatusOpenShop     Status = "open_shop"     // the requester replied; the shop acts next
	StatusClosed       Status = "closed"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusNew, StatusOpenCustomer, StatusOpenShop, StatusClosed:
		return true
	}
	return false
}

// Closed reports whether s is the terminal status.
func (s Status) Closed() bool { return s == StatusClosed }

// Topic classifies a request. It is metadata for filtering and templates.
type Topic string

// Topics (platform catalogue, MVP).
const (
	TopicAllergen Topic = "allergen"
	TopicDelivery Topic = "delivery"
	TopicOrder    Topic = "order"
	TopicPayment  Topic = "payment"
	TopicFeedback Topic = "feedback"
	TopicOther    Topic = "other"
)

// Topics lists every topic.
func Topics() []Topic {
	return []Topic{TopicAllergen, TopicDelivery, TopicOrder, TopicPayment, TopicFeedback, TopicOther}
}

// Valid reports whether t is in the catalogue.
func (t Topic) Valid() bool {
	for _, c := range Topics() {
		if t == c {
			return true
		}
	}
	return false
}

// UserType says who opened a request.
type UserType string

// User types. Claiming turns a visitor request into a customer one.
const (
	UserCustomer UserType = "customer"
	UserVisitor  UserType = "visitor"
)

// AuthorType says who wrote a message.
type AuthorType string

// Author types. Staff are shop_admin only in the MVP; system messages are
// the sweeper's.
const (
	AuthorCustomer  AuthorType = "customer"
	AuthorVisitor   AuthorType = "visitor"
	AuthorShopAdmin AuthorType = "shop_admin"
	AuthorSystem    AuthorType = "system"
)

// FromRequester reports whether the author is the customer or visitor who
// opened the request.
func (a AuthorType) FromRequester() bool { return a == AuthorCustomer || a == AuthorVisitor }

// ClosedReason says why a request was closed.
type ClosedReason string

// Closed reasons. Customers cannot close in the MVP.
const (
	ClosedStaff ClosedReason = "staff"
	ClosedIdle  ClosedReason = "idle"
)

// ErrClosed is the 409 for any post to, or close of, a closed request.
func ErrClosed() error {
	return ycerrors.Conflict("This support request is closed")
}

// StatusAfterPost returns the status a request moves to when author posts a
// message. A closed request accepts nothing.
//
//	new           shop    -> open_customer   customer -> new (shop still to answer)
//	open_customer shop    -> open_customer   customer -> open_shop
//	open_shop     shop    -> open_customer   customer -> open_shop
func StatusAfterPost(s Status, author AuthorType) (Status, error) {
	if s.Closed() {
		return s, ErrClosed()
	}
	if !s.Valid() {
		return s, fmt.Errorf("domain: unknown status %q", s)
	}
	switch {
	case author == AuthorShopAdmin:
		return StatusOpenCustomer, nil
	case author.FromRequester():
		if s == StatusNew {
			return StatusNew, nil
		}
		return StatusOpenShop, nil
	}
	return s, fmt.Errorf("domain: %q cannot post a message", author)
}
