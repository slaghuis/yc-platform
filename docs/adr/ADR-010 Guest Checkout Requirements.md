# ADR-010: Guest Checkout Requirements
**Status**: Accepted 
**Date**: 2025-01-15 
**Context**: Visitors can check out without registering. We need to define the minimum information required for a valid guest order.

## Decision
A guest checkout can only complete if the following are collected during the saga:
 - Email address (for order confirmation and receipts)
 - Telephone number (for shipping notifications and delivery coordination)
 - Delivery address (shipping destination)

## Rules
These fields are validated at the ORDER_CREATE step of the checkout saga.
 - yc-checkout rejects the confirmation request if any required field is missing or fails validation.
 - Phone number is validated with a format check (E.164); SMS verification is not required in v1.
 - Email is validated with a format check; email verification is not required to complete the order (verification links are sent post-order and encourage registration).
 - These fields are stored on the Order record in yc-orders, not as a user record.
 - yc-message uses the guest's phone/email to send order lifecycle notifications.
 - If a guest later registers with the same email, yc-identity's registration flow offers to associate past guest orders with the new customer (see yc-identity requirements, future ADR).
## Digital-Only Orders
 - If every item in the checkout is a digital product (see ADR-012), the delivery address requirement is waived. Email remains mandatory (delivery channel); phone remains mandatory (fraud and account recovery).
 - Rejected PII at Guest Checkout
 - No date of birth
 - No marketing opt-in required (but offered)
 - No password set (user remains a visitor; registration is a separate opt-in post-purchase)
