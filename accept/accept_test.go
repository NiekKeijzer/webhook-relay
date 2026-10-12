package accept_test

import (
	"testing"

	"github.com/NiekKeijzer/webhook-relay/accept"
	defaultaccept "github.com/NiekKeijzer/webhook-relay/accept/default"
)

func TestRegistry(t *testing.T) {
	r, err := accept.NewRegistry(defaultaccept.Type)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Lookup("default"); !ok || got.Name != "default" {
		t.Errorf("Lookup = %+v, %v", got, ok)
	}
	if _, ok := r.Lookup("magic"); ok {
		t.Error("unknown type found")
	}
}

func TestRegistryRejectsDuplicatesAndIncompleteTypes(t *testing.T) {
	if _, err := accept.NewRegistry(defaultaccept.Type, defaultaccept.Type); err == nil {
		t.Error("duplicate accepted")
	}
	if _, err := accept.NewRegistry(accept.Type{Name: "x"}); err == nil {
		t.Error("incomplete type accepted")
	}
}
