package discover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/gotool"
)

// LocalModuleRoots uses go env once so workspace state matches this run.
func LocalModuleRoots(
	ctx context.Context,
	tool gotool.Tool,
	module gotool.Module,
) ([]string, error) {
	environment, err := tool.Environment(ctx, module.Dir)
	if err != nil {
		return nil, err
	}
	return LocalModuleRootsFromEnvironment(ctx, tool, module, environment)
}

// LocalModuleRootsFromEnvironment avoids a second go env for cache salting.
func LocalModuleRootsFromEnvironment(
	ctx context.Context,
	tool gotool.Tool,
	module gotool.Module,
	environment []byte,
) ([]string, error) {
	roots := newRootSet()
	if err := roots.add(module.Dir); err != nil {
		return nil, err
	}
	if GoWorkPath(environment) != "" {
		if err := addWorkspaceRoots(ctx, tool, module.Dir, roots); err != nil {
			return nil, err
		}
	}
	return addReplaceRoots(roots)
}

// rootSet bounds replace walks so cyclic local modules terminate.
type rootSet struct {
	// seen prevents cycles from adding the same filesystem module twice.
	seen map[string]bool
	// roots preserves scan order so newly found replacements are followed.
	roots []string
}

// newRootSet centralizes allocation so callers cannot desync seen and roots.
func newRootSet() *rootSet {
	return &rootSet{seen: make(map[string]bool)}
}

// add validates roots before they reach go-list or hashing.
func (s *rootSet) add(dir string) error {
	root, err := canonicalPath(dir)
	if err != nil {
		return fmt.Errorf("resolving local module root %s: %w", dir, err)
	}
	if s.seen[root] {
		return nil
	}
	if err := validateModuleRoot(root); err != nil {
		return err
	}
	s.seen[root] = true
	s.roots = append(s.roots, root)
	return nil
}

// sorted removes discovery-order noise from later package ordering.
func (s *rootSet) sorted() []string {
	roots := append([]string(nil), s.roots...)
	sort.Strings(roots)
	return roots
}

// addWorkspaceRoots keeps go.work siblings in the same local-root universe.
func addWorkspaceRoots(
	ctx context.Context,
	tool gotool.Tool,
	moduleDir string,
	roots *rootSet,
) error {
	modules, err := tool.ListModules(ctx, moduleDir)
	if err != nil {
		return fmt.Errorf("enumerating workspace modules from %s: %w", moduleDir, err)
	}
	for _, module := range modules {
		if module.Dir == "" {
			return fmt.Errorf("workspace module %s has no directory", module.Path)
		}
		if err := roots.add(module.Dir); err != nil {
			return err
		}
	}
	return nil
}

// addReplaceRoots follows transitive local replaces before sorting the set.
func addReplaceRoots(roots *rootSet) ([]string, error) {
	for index := 0; index < len(roots.roots); index++ {
		replacements, err := LocalReplaceRoots(roots.roots[index])
		if err != nil {
			return nil, err
		}
		for _, replacement := range replacements {
			if err := roots.add(replacement); err != nil {
				return nil, err
			}
		}
	}
	return roots.sorted(), nil
}

// validateModuleRoot fails near the bad root instead of deep in discovery.
func validateModuleRoot(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("local module root %s is unavailable: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local module root %s is not a directory", root)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("local module root %s lacks go.mod: %w", root, err)
	}
	return nil
}

// canonicalPath gives equality checks a single absolute path spelling.
func canonicalPath(dir string) (string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	assert.True(filepath.IsAbs(root), "canonical root %s must be absolute", root)
	return root, nil
}
