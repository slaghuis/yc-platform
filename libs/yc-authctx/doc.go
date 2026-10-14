// Package authctx propagates the authenticated actor context through requests
// and events. yc-gateway populates it from the access token; services read it
// via FromContext.
package authctx