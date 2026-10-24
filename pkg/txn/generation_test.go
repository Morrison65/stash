package txn

import (
	"context"
	"testing"
)

func TestBumpAfterCommit(t *testing.T) {
	gen, bg := Generation(), BackgroundGeneration()

	bumpAfterCommit(context.Background())
	if Generation() != gen+1 || BackgroundGeneration() != bg {
		t.Errorf("a request write must bump the generation only")
	}

	bumpAfterCommit(WithBackground(context.Background()))
	if Generation() != gen+1 || BackgroundGeneration() != bg+1 {
		t.Errorf("a job write must bump the background generation only")
	}
}
