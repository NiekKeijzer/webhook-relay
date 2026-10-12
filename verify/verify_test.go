package verify_test

import (
	"testing"

	"github.com/NiekKeijzer/webhook-relay/verify"
	"github.com/NiekKeijzer/webhook-relay/verify/githubhmac"
	"github.com/NiekKeijzer/webhook-relay/verify/none"
)

func TestRegistry(t *testing.T) {
	r, err := verify.NewRegistry(none.Type, githubhmac.Type)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Lookup("github-hmac-sha256"); !ok || got.Name != "github-hmac-sha256" {
		t.Errorf("Lookup = %+v, %v", got, ok)
	}
	if _, ok := r.Lookup("magic"); ok {
		t.Error("unknown type found")
	}
}

func TestRegistryRejectsDuplicatesAndIncompleteTypes(t *testing.T) {
	if _, err := verify.NewRegistry(none.Type, none.Type); err == nil {
		t.Error("duplicate accepted")
	}
	if _, err := verify.NewRegistry(verify.Type{Name: "x"}); err == nil {
		t.Error("incomplete type accepted")
	}
}
