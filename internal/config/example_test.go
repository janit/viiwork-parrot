package config

import (
	"testing"

	"github.com/janit/viiwork-parrot/internal/catalog"
)

// TestShippedExampleParses: deploy/viiwork-parrot.yaml.example must stay a valid
// config, and every models.want id in it must exist in the repo's
// catalog.yaml (the daemon refuses unknown ids at startup).
func TestShippedExampleParses(t *testing.T) {
	c, err := Load("../../deploy/viiwork-parrot.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	models, err := catalog.LoadYAML("../../catalog.yaml")
	if err != nil || len(models) == 0 {
		t.Fatalf("catalog.yaml: %v (%d models)", err, len(models))
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if len(c.Models.Want) == 0 {
		t.Fatal("example should show a want list")
	}
	for _, w := range c.Models.Want {
		if w != "*" && !ids[w] {
			t.Errorf("example models.want %q is not in catalog.yaml", w)
		}
	}
}
