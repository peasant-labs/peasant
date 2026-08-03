package e2e

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func schemaContractModulePath() string {
	return "github.com/peasant-labs/schema"
}

func schemaModuleParity(peasantGoModPath, villageGoModPath string) (string, error) {
	peasantVersion, err := schemaModuleVersion(peasantGoModPath)
	if err != nil {
		return "", fmt.Errorf("read peasant schema module version: %w", err)
	}
	villageVersion, err := schemaModuleVersion(villageGoModPath)
	if err != nil {
		return "", fmt.Errorf("read village schema module version: %w", err)
	}
	if peasantVersion != villageVersion {
		return "", fmt.Errorf("schema module mismatch: peasant uses %s while village uses %s", peasantVersion, villageVersion)
	}
	return peasantVersion, nil
}

func schemaModuleVersion(goModPath string) (string, error) {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", goModPath, err)
	}

	inRequireBlock := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if inRequireBlock {
			if fields[0] == ")" {
				inRequireBlock = false
				continue
			}
			if len(fields) >= 2 && fields[0] == schemaContractModulePath() {
				return fields[1], nil
			}
			continue
		}
		if fields[0] != "require" {
			continue
		}
		if len(fields) == 2 && fields[1] == "(" {
			inRequireBlock = true
			continue
		}
		if len(fields) >= 3 && fields[1] == schemaContractModulePath() {
			return fields[2], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", goModPath, err)
	}
	return "", fmt.Errorf("%s has no require directive for %s", goModPath, schemaContractModulePath())
}

func assertSchemaModuleParity(t *testing.T, villageBackendDir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "schema module parity check failed",
			what:  "the harness could not resolve its working directory",
			why:   err.Error(),
			where: "internal/e2e/schema_parity.go assertSchemaModuleParity",
			when:  "before building or launching the cross-repo E2E stack",
			means: "the peasant go.mod cannot be located, so consumer contract parity is unknown",
			fix:   "run the harness from inside the peasant checkout and confirm the working directory is readable",
		})
	}
	peasantRoot := peasantRepoRoot(wd)
	if peasantRoot == "" {
		fatalActionable(t, actionableFailure{
			title: "schema module parity check failed",
			what:  fmt.Sprintf("the peasant repository root could not be found from %s", wd),
			why:   "no ancestor go.mod declares module github.com/peasant-labs/peasant",
			where: "internal/e2e/schema_parity.go assertSchemaModuleParity",
			when:  "before building or launching the cross-repo E2E stack",
			means: "the peasant schema-module pin cannot be compared with Village",
			fix:   "run the harness from the peasant checkout or restore its root go.mod",
		})
	}
	version, err := schemaModuleParity(
		filepath.Join(peasantRoot, "go.mod"),
		filepath.Join(villageBackendDir, "go.mod"),
	)
	if err != nil {
		fatalActionable(t, actionableFailure{
			title: "schema module parity check failed",
			what:  "peasant and Village did not prove the same github.com/peasant-labs/schema requirement",
			why:   err.Error(),
			where: "internal/e2e/schema_parity.go assertSchemaModuleParity",
			when:  "after resolving the Village backend and before building either Village binary",
			means: "a green cross-repo run would exercise different wire contracts and be ambiguous",
			fix:   "point VILLAGE_REPO or VILLAGE_BACKEND_DIR at the intended pinned checkout, or re-pin both consumers to the same schema module version",
		})
	}
	t.Logf("schema module parity: %s", version)
}

func peasantRepoRoot(start string) string {
	for dir := start; ; dir = filepath.Dir(dir) {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module github.com/peasant-labs/peasant") {
			return dir
		}
		if parent := filepath.Dir(dir); parent == dir {
			return ""
		}
	}
}
