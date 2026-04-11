package packs

import "testing"

// matchLibID is unexported, so this test sits in the same package
// (no _test suffix) to exercise it directly. The same scenarios are
// covered indirectly by TestFilterByLibID in manifest_test.go, but
// having a unit-level test makes regression triage faster.
func TestMatchLibID(t *testing.T) {
	cases := []struct {
		libID  string
		filter string
		want   bool
		why    string
	}{
		{"/x/y", "/x/y", true, "exact match"},
		{"/x/y/v1", "/x/y", true, "versioned child of base"},
		{"/x/y/v18", "/x/y", true, "second versioned child"},
		{"/x/y", "/x/y/v1", false, "base does not match versioned filter"},
		{"/x/yother", "/x/y", false, "prefix that is NOT /-bounded"},
		{"/x/y/sub/deeper", "/x/y", true, "/-bounded prefix at any depth"},
		{"/totally/different", "/x/y", false, "unrelated"},
		{"/x/y", "/x/y/", false, "trailing slash filter is not normalized"},
	}
	for _, c := range cases {
		got := matchLibID(c.libID, c.filter)
		if got != c.want {
			t.Errorf("matchLibID(%q, %q) = %v, want %v (%s)", c.libID, c.filter, got, c.want, c.why)
		}
	}
}
