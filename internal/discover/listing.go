package discover

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chronark/rask/internal/gotool"
)

// discover keeps the closure build separate from the public shuffle contract.
func (d *discoverer) discover(ctx context.Context) ([]gotool.Package, error) {
	roots, err := LocalModuleRoots(ctx, d.tool, d.module)
	if err != nil {
		return nil, err
	}
	selected, err := d.listSelected(ctx)
	if err != nil {
		return nil, err
	}
	dependencies, err := d.tool.ListDependencies(ctx, d.patternDir, dependencyTargets(selected))
	if err != nil {
		return nil, err
	}
	return d.validateAndMark(localPackages(dependencies, roots), selected)
}

// listSelected fails fast so an empty user pattern cannot look like success.
func (d *discoverer) listSelected(ctx context.Context) ([]gotool.Package, error) {
	selected, err := d.tool.ListPackages(ctx, d.patternDir, d.patterns)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("package pattern %s matched no packages", strings.Join(d.patterns, " "))
	}
	return selected, nil
}

func dependencyTargets(selected []gotool.Package) []string {
	paths := make(map[string]bool)
	for _, p := range selected {
		paths[p.ImportPath] = true
		for _, path := range p.TestImports {
			paths[path] = true
		}
		for _, path := range p.XTestImports {
			paths[path] = true
		}
	}
	delete(paths, "C")
	targets := make([]string, 0, len(paths))
	for path := range paths {
		targets = append(targets, path)
	}
	sort.Strings(targets)
	return targets
}

func localPackages(packages []gotool.Package, roots []string) []gotool.Package {
	var local []gotool.Package
	byPath := make(map[string]gotool.Package, len(packages))
	localPaths := make(map[string]bool)
	for _, p := range packages {
		byPath[p.ImportPath] = p
		p.ModuleDir = ""
		for _, root := range roots {
			relative, err := filepath.Rel(root, p.Dir)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				continue
			}
			if len(root) > len(p.ModuleDir) {
				p.ModuleDir = root
			}
		}
		if p.ModuleDir != "" {
			local = append(local, p)
			localPaths[p.ImportPath] = true
		}
	}
	bridges := make(map[string][]string)
	bridge := func(path string) []string {
		if localPaths[path] {
			return []string{path}
		}
		if paths, ok := bridges[path]; ok {
			return paths
		}
		seen := make(map[string]bool)
		reachable := make(map[string]bool)
		pending := []string{path}
		for len(pending) > 0 {
			current := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if seen[current] {
				continue
			}
			seen[current] = true
			if localPaths[current] {
				reachable[current] = true
				continue
			}
			pending = append(pending, byPath[current].Imports...)
		}
		paths := make([]string, 0, len(reachable))
		for reachablePath := range reachable {
			paths = append(paths, reachablePath)
		}
		sort.Strings(paths)
		bridges[path] = paths
		return paths
	}
	collapse := func(imports []string) []string {
		paths := make(map[string]bool)
		for _, path := range imports {
			for _, target := range bridge(path) {
				paths[target] = true
			}
		}
		collapsed := make([]string, 0, len(paths))
		for path := range paths {
			collapsed = append(collapsed, path)
		}
		sort.Strings(collapsed)
		return collapsed
	}
	for index := range local {
		local[index].Imports = collapse(local[index].Imports)
		local[index].TestImports = collapse(local[index].TestImports)
		local[index].XTestImports = collapse(local[index].XTestImports)
	}
	return local
}
