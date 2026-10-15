package discover

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/gotool"
)

// discoverer keeps one discovery pass explicit while Packages stays small.
type discoverer struct {
	// tool is the only boundary allowed to invoke the Go command.
	tool gotool.Tool
	// module scopes user patterns to the current module contract.
	module gotool.Module
	// patternDir preserves the caller's working directory for relative patterns.
	patternDir string
	// patterns are the user package patterns whose matches become selected.
	patterns []string
	// rand is injected in tests to prove shuffling without disabling it.
	rand *rand.Rand
}

// Packages returns selected packages and their local build and test dependencies.
func Packages(
	ctx context.Context,
	tool gotool.Tool,
	module gotool.Module,
	patternDir string,
	patterns []string,
) ([]gotool.Package, error) {
	return packagesWithRand(ctx, tool, module, patternDir, patterns, randomRand())
}

// packagesWithRand keeps shuffling observable without weakening production runs.
func packagesWithRand(
	ctx context.Context,
	tool gotool.Tool,
	module gotool.Module,
	patternDir string,
	patterns []string,
	rng *rand.Rand,
) ([]gotool.Package, error) {
	assert.NotNil(tool, "discovery needs a go tool")
	assert.NotNil(rng, "discovery needs a random source")
	discoverer := &discoverer{
		tool:       tool,
		module:     module,
		patternDir: patternDir,
		patterns:   patterns,
		rand:       rng,
	}
	packages, err := discoverer.discover(ctx)
	if err != nil {
		return nil, err
	}
	shufflePackages(packages, discoverer.rand)
	return packages, nil
}

// randomRand returns a process-local source so every run shuffles by default.
func randomRand() *rand.Rand {
	seed := uint64(time.Now().UnixNano())
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// shufflePackages changes only return order, never closure contents.
func shufflePackages(packages []gotool.Package, rng *rand.Rand) {
	rng.Shuffle(len(packages), func(i int, j int) {
		packages[i], packages[j] = packages[j], packages[i]
	})
}
