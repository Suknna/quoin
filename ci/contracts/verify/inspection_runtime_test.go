package main

import "testing"

func TestMixedInspectionPromQLSQLiteClosure(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyMixedInspectionPromQLSQLite(root); err != nil {
		t.Fatal(err)
	}
}
