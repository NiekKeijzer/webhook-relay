package defaultaccept

import (
	"testing"

	"github.com/NiekKeijzer/webhook-relay/config"
	"github.com/NiekKeijzer/webhook-relay/plugin"
)

func TestDefault(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		status int
	}{
		{"no params", nil, 202},
		{"status 200", map[string]any{"status": int64(200)}, 200},
		{"status 204", map[string]any{"status": int64(204)}, 204},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := config.NewParams(tt.params)
			if err := Type.Validate(p); err != nil {
				t.Fatal(err)
			}
			a, err := Type.New(p)
			if err != nil {
				t.Fatal(err)
			}
			d := a.Accept(&plugin.Request{Method: "POST"})
			if !d.Enqueue || d.Status != tt.status || len(d.Body) != 0 || len(d.Header) != 0 {
				t.Errorf("decision = %+v", d)
			}
		})
	}
}

func TestDefaultRejects(t *testing.T) {
	for name, params := range map[string]map[string]any{
		"status 0":      {"status": int64(0)},
		"status 199":    {"status": int64(199)},
		"status 300":    {"status": int64(300)},
		"status 500":    {"status": int64(500)},
		"status string": {"status": "202"},
		"unknown key":   {"code": int64(202)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Type.Validate(config.NewParams(params)); err == nil {
				t.Error("accepted")
			}
			if _, err := Type.New(config.NewParams(params)); err == nil {
				t.Error("New accepted")
			}
		})
	}
}
