// Copyright (c) 2026 Michael D Henderson.

// Package edge holds the two things both HTTP transports must agree on.
//
// DESIGN.md 14 says mapping a domain error to an HTTP status happens "only at
// the transport edge, in one function", and DESIGN.md 12's session cookie is
// written by "one cookie-writing path in the process, not two". Until M13
// there was one transport, so both lived in internal/api and the rules held by
// construction. M13 adds internal/web, and a second copy of either would be a
// second policy: two functions that disagree about what "conflict" means, or a
// second cookie that is missing Secure (invariant 13).
//
// A sibling import would have been the other answer, and the design rejects it
// for the same reason internal/reqctx exists: api, web, web/devroutes and
// server agree about a value by importing a leaf, never one another
// (DESIGN.md 4). So this package is a leaf. It imports the standard library
// and internal/{domain,config} and nothing else, holds no state, and must
// never grow a third responsibility -- a package named for "the edge" is a
// package everything at the edge would otherwise be tempted to put things in.
//
// It renders nothing. The problem document is internal/api's, because RFC 9457
// is the JSON API's contract; the HTML error page is internal/web's. What is
// shared is the decision, not the presentation.
package edge
