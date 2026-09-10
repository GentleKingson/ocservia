package mysql

import "testing"

func TestRevisionObjectSequence(t *testing.T) {
	before, after := digest([]byte("original")), digest([]byte("altered"))
	root := manifest{Steps: []step{{Name: "example", Kind: "table", SchemaHash: before}}}
	plan := revisionPlan{Steps: []revisionStep{
		{Name: "alter_example", Object: "example", Kind: "table", SQL: "alter", Checksum: digest([]byte("alter")), Before: before, After: after},
		{Name: "backfill_example", Kind: "data", SQL: "backfill", Checksum: digest([]byte("backfill")), VerifySQL: "verify", After: digest([]byte("valid")), Repairable: true},
		{Name: "drop_example", Object: "example", Kind: "table", SQL: "drop", Checksum: digest([]byte("drop")), Before: after},
	}}
	got, err := validateRevisionPlan(root, plan)
	if err != nil || len(got.Steps) != 0 {
		t.Fatal(got, err)
	}
	if root.Steps[0].SchemaHash != before {
		t.Fatal("predecessor snapshot mutated")
	}
	plan.Steps[2].Before = before
	if _, err := validateRevisionPlan(root, plan); err == nil {
		t.Fatal("noncontiguous object history accepted")
	}
	plan.Steps[2].Before = after
	plan.Steps[1].After = digest([]byte("invalid"))
	if _, err := validateRevisionPlan(root, plan); err == nil {
		t.Fatal("invalid data postcondition accepted")
	}
}
