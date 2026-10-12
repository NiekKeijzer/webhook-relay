package none

import (
	"net/http"
	"testing"

	"github.com/NiekKeijzer/webhook-relay/config"
	"github.com/NiekKeijzer/webhook-relay/plugin"
)

func TestNone(t *testing.T) {
	if Type.Name != "none" || Type.NeedsSecrets {
		t.Errorf("type = %+v", Type)
	}
	v, err := Type.New(config.NewParams(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(&plugin.Request{Method: "POST", Header: http.Header{}, Body: []byte("anything")}); err != nil {
		t.Errorf("request rejected: %v", err)
	}
	if h := v.Headers(); len(h) != 0 {
		t.Errorf("Headers() = %v", h)
	}
}

func TestNoneRejectsParamsAndSecrets(t *testing.T) {
	if err := Type.Validate(config.NewParams(map[string]any{"x": int64(1)})); err == nil {
		t.Error("params accepted")
	}
	if _, err := Type.New(config.NewParams(nil), [][]byte{[]byte("secret")}); err == nil {
		t.Error("secret accepted")
	}
}
