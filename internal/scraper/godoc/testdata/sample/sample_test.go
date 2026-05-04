package sample

import "testing"

// TestSomething must NOT appear in any chunk — _test.go files are
// filtered before go/doc sees them.
func TestSomething(t *testing.T) {}
