package testcmd

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/chronark/rask/internal/cli"
	"github.com/chronark/rask/internal/gotool"
)

type shardOptions struct {
	index   int
	count   int
	timings *shardTimings
}

type shardTimings struct {
	Source         string             `json:"source,omitempty"`
	DefaultSeconds float64            `json:"default_seconds"`
	Seconds        map[string]float64 `json:"seconds"`
}

func shardOptionsFromCommand(command *cli.Command) (shardOptions, error) {
	var options shardOptions
	if !command.IsSet("shard") {
		if command.IsSet("shard-timings") {
			return options, fmt.Errorf("--shard-timings requires --shard")
		}
		return options, nil
	}
	value := command.String("shard")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || strings.ContainsAny(value, "+- \t\n\r") {
		return options, fmt.Errorf("--shard must be INDEX/COUNT with 1 <= INDEX <= COUNT")
	}
	index, indexErr := strconv.Atoi(parts[0])
	count, countErr := strconv.Atoi(parts[1])
	if indexErr != nil || countErr != nil || index < 1 || count < 1 || index > count {
		return options, fmt.Errorf("--shard must be INDEX/COUNT with 1 <= INDEX <= COUNT")
	}
	options.index, options.count = index, count
	if command.IsSet("shard-timings") {
		body, err := os.ReadFile(command.String("shard-timings"))
		if err != nil {
			return options, fmt.Errorf("reading --shard-timings: %w", err)
		}
		var timings shardTimings
		if err := json.Unmarshal(body, &timings); err != nil {
			return options, fmt.Errorf("decoding --shard-timings: %w", err)
		}
		if !validSeconds(timings.DefaultSeconds) {
			return options, fmt.Errorf("--shard-timings default_seconds must be finite and positive")
		}
		for path, seconds := range timings.Seconds {
			if path == "" || !validSeconds(seconds) {
				return options, fmt.Errorf("--shard-timings seconds[%q] must be finite and positive", path)
			}
		}
		options.timings = &timings
	}
	return options, nil
}

func validSeconds(seconds float64) bool {
	return seconds > 0 && !math.IsInf(seconds, 0) && !math.IsNaN(seconds)
}

func (s shardOptions) seconds(path string) float64 {
	if s.timings == nil {
		return 1
	}
	if seconds, ok := s.timings.Seconds[path]; ok {
		return seconds
	}
	return s.timings.DefaultSeconds
}

func (s shardOptions) less(left, right string) bool {
	if s.seconds(left) != s.seconds(right) {
		return s.seconds(left) > s.seconds(right)
	}
	return left < right
}

func (s shardOptions) selectPackages(packages []gotool.Package) ([]gotool.Package, error) {
	if s.count == 0 {
		return packages, nil
	}
	paths := make([]string, 0, len(packages))
	for _, p := range packages {
		if p.Selected && p.HasTests() {
			paths = append(paths, p.ImportPath)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return s.less(paths[i], paths[j]) })
	members := make(map[string]bool)
	loads := make([]float64, min(s.count, len(paths)))
	for position, path := range paths {
		assigned := position % s.count
		if s.timings != nil {
			assigned = 0
			for shard := range loads {
				if loads[shard] < loads[assigned] {
					assigned = shard
				}
			}
			loads[assigned] += s.seconds(path)
			if math.IsInf(loads[assigned], 0) {
				return nil, fmt.Errorf("--shard-timings total seconds overflow")
			}
		}
		members[path] = assigned+1 == s.index
	}
	selected := append([]gotool.Package(nil), packages...)
	for index := range selected {
		selected[index].Selected = members[selected[index].ImportPath]
	}
	return selected, nil
}
