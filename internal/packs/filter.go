package packs

import "strings"

// matchLibID is the two-level filter shared by `packs download -lib X`
// and `packs list -lib X`. It mirrors the scraper's behaviour from
// internal/scraper/config.go: an exact match returns true, and a base
// lib_id matches every versioned child by treating the filter as a
// /-suffixed prefix.
//
//	matchLibID("/x/y",     "/x/y")    → true
//	matchLibID("/x/y/v1",  "/x/y")    → true   (versioned child)
//	matchLibID("/x/y/v18", "/x/y")    → true
//	matchLibID("/x/yother","/x/y")    → false  (NOT a /-bounded prefix)
//	matchLibID("/x/y",     "")        → caller's job; this fn assumes
//	                                    the empty filter is handled
//	                                    upstream by FilterByLibID
//
// Lives in its own file so cmd/packs and any future tooling can call
// it without depending on the Manifest type.
func matchLibID(libID, filter string) bool {
	if libID == filter {
		return true
	}
	return strings.HasPrefix(libID, filter+"/")
}
