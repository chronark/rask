package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestShardsExactOnceAcrossCacheStates(t *testing.T) {
	for _, balanced := range []bool{false, true} {
		name := "round-robin"
		if balanced {
			name = "balanced"
		}
		t.Run(name, func(t *testing.T) {
			module := scaffoldModule(t,
				packageSpec{name: "bystander", test: testPass},
				packageSpec{name: "dep", test: testPass},
				packageSpec{name: "dependent", imports: []string{"dep"}, test: testPass},
				packageSpec{name: "extra", test: testPass},
				packageSpec{name: "skip", test: testSkip},
				packageSpec{name: "slow", test: testPass},
				packageSpec{name: "tail", test: testPass},
				packageSpec{name: "utility", test: testNone},
			)
			remote, _ := startFakeS3(t)
			warmEnv := envWithCache(t.TempDir(), remote)
			baseline := runRask(t, module.dir, warmEnv, "./...")
			assertCode(t, baseline, 0)
			wantAll := []string{"bystander", "dep", "dependent", "extra", "skip", "slow", "tail"}
			if got := reportedSuites(baseline.stdout); !reflect.DeepEqual(got, wantAll) {
				t.Fatalf("unsharded suites=%v, want %v\n%s", got, wantAll, baseline.stdout)
			}
			flags := []string{}
			want := [][]string{{"bystander", "extra", "tail"}, {"dep", "skip"}, {"dependent", "slow"}}
			if balanced {
				snapshot := struct {
					DefaultSeconds float64            `json:"default_seconds"`
					Seconds        map[string]float64 `json:"seconds"`
				}{DefaultSeconds: 2, Seconds: map[string]float64{
					module.path + "/tail": 9, module.path + "/dep": 8,
					module.path + "/dependent": 7, module.path + "/extra": 6,
					module.path + "/skip": 5,
				}}
				body, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, module.dir, "timings.json", string(body))
				flags = append(flags, "--shard-timings", filepath.Join(module.dir, "timings.json"))
				want = [][]string{{"bystander", "slow", "tail"}, {"dep", "skip"}, {"dependent", "extra"}}
			}
			envs := []map[string]string{cacheEnv(t.TempDir()), warmEnv, envWithCache(t.TempDir(), remote)}
			for round := 0; round < 3; round++ {
				var all []string
				for index, env := range envs {
					var args []string
					if round == 1 {
						args = append(args, "test")
					} else if round == 2 {
						args = append(args, "t")
					}
					args = append(args, flags...)
					args = append(args, "--concurrency=1", "--shard", fmt.Sprintf("%d/3", index+1))
					if round == 1 {
						args = append(args, "--no-cache")
					}
					if round == 2 {
						args = append(args, "./tail", "./slow", "./skip", "./extra", "./dependent", "./dep", "./bystander", "./utility", "./dep")
					} else {
						args = append(args, "./...")
					}
					result := runRask(t, module.dir, env, args...)
					assertCode(t, result, 0)
					got := reportedSuites(result.stdout)
					t.Logf("round=%d shard=%d/3 suites=%v\n%s", round, index+1, got, result.stdout)
					if !reflect.DeepEqual(got, want[index]) {
						t.Fatalf("shard %d suites=%v, want %v", index+1, got, want[index])
					}
					if balanced && index == 0 && round == 1 {
						if strings.Index(result.stdout, "Passed tail") > strings.Index(result.stdout, "Passed bystander") {
							t.Fatal("longest suite did not run first with concurrency=1")
						}
					}
					if round == 0 {
						verb := "Cached"
						if index == 0 {
							verb = "Passed"
						}
						assertSuites(t, result.stdout, verb, want[index]...)
						if index == 2 {
							assertContains(t, result.stdout, fmt.Sprintf("%d remote", len(want[index])))
						}
					}
					all = append(all, got...)
				}
				sort.Strings(all)
				if !reflect.DeepEqual(all, wantAll) {
					t.Fatalf("union=%v, want exact-once %v", all, wantAll)
				}
			}
			full := runRask(t, module.dir, warmEnv, "./...")
			assertCode(t, full, 0)
			assertSuites(t, full.stdout, "Cached", wantAll...)
			editDepSource(t, module)
			var changed string
			for index := range envs {
				args := append(append([]string{}, flags...), "--shard", fmt.Sprintf("%d/3", index+1), "./...")
				result := runRask(t, module.dir, warmEnv, args...)
				assertCode(t, result, 0)
				if got := reportedSuites(result.stdout); !reflect.DeepEqual(got, want[index]) {
					t.Fatalf("membership changed after dependency edit: %v, want %v", got, want[index])
				}
				changed += result.stdout
			}
			assertSuites(t, changed, "Passed", "dep", "dependent")
			assertSuites(t, changed, "Cached", "bystander", "extra", "skip", "slow", "tail")
		})
	}
}

func TestShardFailFastAndEmpty(t *testing.T) {
	module := scaffoldModule(t,
		packageSpec{name: "failer", test: testFail},
		packageSpec{name: "later", test: testPass},
	)
	result := runRask(t, module.dir, cacheEnv(t.TempDir()), "--shard=1/1", "--concurrency=1", "./...")
	assertCode(t, result, 1)
	assertContains(t, result.stdout, "intentional failure")
	assertNotContains(t, result.stdout, "Passed later")
	empty := runRask(t, module.dir, cacheEnv(t.TempDir()), "--shard=3/3", "./...")
	assertCode(t, empty, 0)
	if got := reportedSuites(empty.stdout); len(got) != 0 {
		t.Fatalf("empty shard suites=%v", got)
	}
	large := runRask(t, module.dir, cacheEnv(t.TempDir()), "--shard=2147483647/2147483647", "./...")
	assertCode(t, large, 0)
	writeFile(t, module.dir, "later/later.go", "package later\nfunc Value() int { return -1 }\n")
	failure := runRask(t, module.dir, cacheEnv(t.TempDir()), "--shard=2/2", "./...")
	assertCode(t, failure, 1)
	assertContains(t, failure.stdout, "bad value")
}

func TestShardInvalidInputs(t *testing.T) {
	for _, value := range []string{"", "0/4", "5/4", "1/0", "-1/4", "1/-4", "1", "1/2/3", "a/4", "+1/4", "1 /4", "1/99999999999999999999999999999"} {
		result := runRask(t, t.TempDir(), cacheEnv(t.TempDir()), "--shard="+value)
		assertCode(t, result, 1)
		assertContains(t, result.stderr, "--shard must be")
	}
	for _, args := range [][]string{{"--shard"}, {"--shard-timings"}, {"--shard-timings=x"}, {"--shard=1/2", "--shard-timings="}} {
		result := runRask(t, t.TempDir(), cacheEnv(t.TempDir()), args...)
		assertCode(t, result, 1)
		assertNotContains(t, result.stderr, "go list")
	}
	for _, body := range []string{"{", "null", "{}", `{"default_seconds":0}`, `{"default_seconds":-1}`, `{"default_seconds":1,"seconds":{"p":-2}}`, `{"default_seconds":1,"seconds":{"p":0}}`, `{"default_seconds":1e999}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "timings.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		result := runRask(t, dir, cacheEnv(t.TempDir()), "--shard=1/2", "--shard-timings", path)
		assertCode(t, result, 1)
		assertContains(t, result.stderr, "--shard-timings")
	}
}

func reportedSuites(output string) []string {
	var names []string
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[0] == "Passed" || fields[0] == "Cached") {
			names = append(names, fields[1])
		}
	}
	sort.Strings(names)
	return names
}
