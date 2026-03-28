package chain

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadDir loads all chain YAML files from a directory.
func LoadDir(dir string) ([]*ChainDef, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read chain dir: %w", err)
	}

	var chains []*ChainDef
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
			continue
		}
		def, err := LoadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue // skip invalid files
		}
		chains = append(chains, def)
	}
	return chains, nil
}

// LoadFile loads a single chain YAML file.
func LoadFile(path string) (*ChainDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var def ChainDef
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("parse chain %s: %w", path, err)
	}
	if def.Name == "" {
		// Derive name from filename
		base := filepath.Base(path)
		def.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if len(def.Steps) == 0 {
		return nil, fmt.Errorf("chain %s has no steps", def.Name)
	}
	return &def, nil
}
