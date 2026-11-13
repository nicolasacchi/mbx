// Package aliases manages user-controlled card name → ID mappings.
//
// Stored at ~/.config/mbx/aliases.toml. Refreshed by `mbx aliases sync`,
// which queries Metabase's /api/search and writes the file under user control.
// No alias data is hardcoded into the binary.
package aliases

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	cliconfig "github.com/nicolasacchi/clicore/config"
)

// Alias is one entry. Alias key is the lowercase slug.
type Alias struct {
	Alias      string `toml:"alias" json:"alias"`
	CardID     int    `toml:"card_id" json:"card_id"`
	DatabaseID int    `toml:"database_id" json:"database_id"`
	Name       string `toml:"name" json:"name"`
	UpdatedAt  string `toml:"updated_at" json:"updated_at"`
}

// Store is the on-disk shape of aliases.toml.
type Store struct {
	UpdatedAt string           `toml:"updated_at"`
	Aliases   map[string]Alias `toml:"aliases"`
}

// Path returns the aliases file path: $XDG_CONFIG_HOME/mbx/aliases.toml.
func Path() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "mbx", "aliases.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "mbx", "aliases.toml"), nil
}

// Load reads aliases.toml. Missing file returns an empty Store.
func Load() (*Store, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{Aliases: map[string]Alias{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s Store
	if err := toml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Aliases == nil {
		s.Aliases = map[string]Alias{}
	}
	return &s, nil
}

// Save writes the store to aliases.toml (mode 0600).
func Save(s *Store) error {
	path, err := Path()
	if err != nil {
		return err
	}
	// Atomic temp+rename (clicore) — replaces the O_TRUNC write so an
	// interrupted encode can't corrupt the alias store.
	return cliconfig.SaveTOML(path, s)
}

// Find looks up an alias by case-insensitive name.
func (s *Store) Find(name string) (Alias, bool) {
	key := slug(name)
	if a, ok := s.Aliases[key]; ok {
		return a, true
	}
	return Alias{}, false
}

// Set inserts or replaces an alias.
func (s *Store) Set(a Alias) {
	if s.Aliases == nil {
		s.Aliases = map[string]Alias{}
	}
	s.Aliases[slug(a.Alias)] = a
}

// All returns aliases in deterministic (alphabetical) order.
func (s *Store) All() []Alias {
	keys := make([]string, 0, len(s.Aliases))
	for k := range s.Aliases {
		keys = append(keys, k)
	}
	// simple insertion sort — alias counts are small
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	out := make([]Alias, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.Aliases[k])
	}
	return out
}

// ParseInt returns (n, true) iff s is purely a non-negative integer.
func ParseInt(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c == ' ' || c == '_' || c == '-' || c == '/' || c == '|':
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	return strings.Trim(string(out), "-")
}

// Slug exposes the slug helper for the commands layer.
func Slug(s string) string { return slug(s) }
