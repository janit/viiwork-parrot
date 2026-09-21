package node

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// viiworkModelPaths returns the model file paths a viiwork config loads:
// v1 `model.path` and v2 `models[].path`. Unknown keys are ignored.
func viiworkModelPaths(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var c struct {
		Model struct {
			Path string `yaml:"path"`
		} `yaml:"model"`
		Models []struct {
			Path string `yaml:"path"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var out []string
	if c.Model.Path != "" {
		out = append(out, c.Model.Path)
	}
	for _, m := range c.Models {
		if m.Path != "" {
			out = append(out, m.Path)
		}
	}
	return out, nil
}
