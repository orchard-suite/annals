package gate

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/orchard-suite/platformkit/v3/change"
)

// ReadSettings reads how the gate is scored from the environment:
// GATE_PACK, the estate's gate pack, whose kinds list names the estate's
// own effect kinds and their families; and GATE_MAX_BLIND, the percent of
// the changes that ran a family's watch may fail to see and still be
// scored. With no pack, only the suite's base kinds are known.
func ReadSettings(getenv func(string) string) (Settings, error) {
	s := Settings{Kinds: change.Base(), MaxBlind: 20}
	if path := getenv("GATE_PACK"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return s, fmt.Errorf("GATE_PACK: %w", err)
		}
		if s.Kinds, err = change.PackKinds(data); err != nil {
			return s, fmt.Errorf("GATE_PACK %s: %w", path, err)
		}
	}
	if v := getenv("GATE_MAX_BLIND"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return s, errors.New("GATE_MAX_BLIND must be a whole percent from 0 to 100")
		}
		s.MaxBlind = n
	}
	return s, nil
}
