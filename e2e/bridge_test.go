package e2e

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func TestExternalBridgeInvalidatesLocalSource(t *testing.T) {
	for _, kind := range []string{"build", "test", "xtest"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			app := filepath.Join(root, "app")
			proxy := filepath.Join(root, "proxy")
			files := map[string]string{
				"go.mod":      "module example.test/external\n\ngo 1.25\nrequire example.test/local v0.0.0\n",
				"external.go": "package external\nimport \"example.test/local\"\nfunc Value() int { return local.Value() }\n",
			}
			var archive bytes.Buffer
			writer := zip.NewWriter(&archive)
			for _, name := range []string{"go.mod", "external.go"} {
				entry, err := writer.Create("example.test/external@v1.0.0/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := entry.Write([]byte(files[name])); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			writeFile(t, proxy, "example.test/external/@v/v1.0.0.zip", archive.String())
			writeFile(t, proxy, "example.test/external/@v/v1.0.0.mod", files["go.mod"])
			info, err := json.Marshal(struct {
				Version string
				Time    string
			}{Version: "v1.0.0", Time: "2026-01-01T00:00:00Z"})
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, proxy, "example.test/external/@v/v1.0.0.info", string(info))
			writeFile(t, app, "go.mod", "module example.test/app\n\ngo 1.25\nrequire (\nexample.test/external v1.0.0\nexample.test/local v0.0.0\n)\nreplace example.test/local => ../local\n")
			writeFile(t, root, "local/go.mod", "module example.test/local\n\ngo 1.25\n")
			writeFile(t, root, "local/local.go", "package local\nfunc Value() int { return 1 }\n")
			writeFile(t, app, "suite/suite.go", "package suite\n")
			pkg, expression, testImport := "suite", "external.Value()", "\"example.test/external\""
			if kind == "build" {
				writeFile(t, app, "suite/suite.go", "package suite\nimport \"example.test/external\"\nfunc Value() int { return external.Value() }\n")
				expression, testImport = "Value()", ""
			} else if kind == "xtest" {
				pkg = "suite_test"
			}
			writeFile(t, app, "suite/suite_test.go", fmt.Sprintf("package %s\nimport (\"testing\"; %s)\nfunc TestValue(t *testing.T) { if %s != 1 { t.Fatal(\"local source changed\") } }\n", pkg, testImport, expression))
			writePackage(t, app, "example.test/app", packageSpec{name: "bystander", test: testPass})
			env := cacheEnv(t.TempDir())
			env["GOPROXY"], env["GOSUMDB"] = "file://"+proxy, "off"
			env["GOMODCACHE"] = filepath.Join(root, "modcache")
			env["GOFLAGS"] = "-mod=mod -modcacherw"
			cold := runRask(t, app, env, "./...")
			assertCode(t, cold, 0)
			assertSuites(t, cold.stdout, "Passed", "suite", "bystander")
			warm := runRask(t, app, env, "./...")
			assertCode(t, warm, 0)
			assertSuites(t, warm.stdout, "Cached", "suite", "bystander")
			writeFile(t, root, "local/local.go", "package local\nfunc Value() int { return 2 }\n")
			changed := runRask(t, app, env, "./...")
			t.Logf("after local edit: exit=%d\n%s%s", changed.code, changed.stdout, changed.stderr)
			assertCode(t, changed, 1)
			assertContains(t, changed.stdout+changed.stderr, "local source changed")
			assertSuites(t, changed.stdout, "Cached", "bystander")
		})
	}
}
