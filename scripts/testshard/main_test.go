package main

import (
	_ "embed"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/plan_cases.yaml
var planCasesYAML []byte

type planCaseTest struct {
	Name     string `yaml:"name"`
	WallMS   int64  `yaml:"wall_ms"`
	EnvBound bool   `yaml:"env_bound"`
	Serial   bool   `yaml:"serial"`
}

type planCase struct {
	Name   string         `yaml:"name"`
	Shards int            `yaml:"shards"`
	Tests  []planCaseTest `yaml:"tests"`
}

type planFixture struct {
	RequiredNames []string   `yaml:"required_names"`
	Mutations     []string   `yaml:"mutations"`
	Cases         []planCase `yaml:"cases"`
}

func loadPlanFixtures(t *testing.T) planFixture {
	t.Helper()
	var fixture planFixture
	if err := yaml.Unmarshal(planCasesYAML, &fixture); err != nil {
		t.Fatalf("decode plan fixture: %v", err)
	}
	present := map[string]bool{}
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	if len(fixture.RequiredNames) == 0 || len(fixture.Mutations) == 0 {
		t.Fatal("plan fixture declares an empty required manifest")
	}
	for _, name := range fixture.RequiredNames {
		if !present[name] {
			t.Fatalf("plan fixture is missing required case %q", name)
		}
	}
	return fixture
}

func inputs(c planCase) (names []string, costs map[string]int64, envBound, serial map[string]bool) {
	costs = map[string]int64{}
	envBound = map[string]bool{}
	serial = map[string]bool{}
	for _, test := range c.Tests {
		names = append(names, test.Name)
		costs[test.Name] = test.WallMS
		if test.EnvBound {
			envBound[test.Name] = true
		}
		if test.Serial {
			serial[test.Name] = true
		}
	}
	return names, costs, envBound, serial
}

// TestBuildPlanIsAnExactPartition drives every fixture case through the computed
// partition and asserts the properties the collision audit exists to protect:
// exact cover, no duplicate, non-empty shards, and per-shard -run regexes that
// match exactly their own tests.
func TestBuildPlanIsAnExactPartition(t *testing.T) {
	fixture := loadPlanFixtures(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			names, costs, envBound, serial := inputs(c)
			plan := buildPlan("example/pkg", c.Shards, names, costs, envBound, serial)
			if err := auditPlan(plan); err != nil {
				t.Fatalf("audit rejected a valid partition: %v", err)
			}
			if plan.Total != len(names) || plan.Covered != len(names) {
				t.Fatalf("plan total=%d covered=%d want %d", plan.Total, plan.Covered, len(names))
			}
			seen := map[string]bool{}
			for _, shard := range plan.ShardSet {
				if len(shard.Tests) == 0 {
					t.Fatalf("shard %d is empty", shard.Index)
				}
				re, err := regexp.Compile(shard.RunRegex)
				if err != nil {
					t.Fatalf("shard %d run regex %q does not compile: %v", shard.Index, shard.RunRegex, err)
				}
				for _, test := range shard.Tests {
					if seen[test.Name] {
						t.Fatalf("test %s appears in more than one shard", test.Name)
					}
					seen[test.Name] = true
					if !re.MatchString(test.Name) {
						t.Fatalf("shard %d regex %q does not match its own test %s", shard.Index, shard.RunRegex, test.Name)
					}
				}
			}
			if len(seen) != len(names) {
				t.Fatalf("shards name %d distinct tests, want %d", len(seen), len(names))
			}
		})
	}
}

// TestAuditRejectsCorruptPartitions is the mutation proof: each named corruption
// must make the collision audit fail, so a green audit is not vacuous.
func TestAuditRejectsCorruptPartitions(t *testing.T) {
	fixture := loadPlanFixtures(t)
	base := fixture.Cases[0]
	names, costs, envBound, serial := inputs(base)
	clean := buildPlan("example/pkg", base.Shards, names, costs, envBound, serial)
	if err := auditPlan(clean); err != nil {
		t.Fatalf("base plan must audit clean: %v", err)
	}
	validMutations := map[string]func(Plan) Plan{}
	validMutations["duplicated-test"] = func(p Plan) Plan {
		moved := p.ShardSet[0].Tests[0]
		p.ShardSet[1].Tests = append(p.ShardSet[1].Tests, moved)
		return p
	}
	validMutations["dropped-test"] = func(p Plan) Plan {
		p.ShardSet[0].Tests = p.ShardSet[0].Tests[1:]
		return p
	}
	validMutations["empty-shard"] = func(p Plan) Plan {
		p.ShardSet[0].Tests = nil
		return p
	}
	for _, name := range fixture.Mutations {
		mutate, ok := validMutations[name]
		if !ok {
			t.Fatalf("mutation %q has no implementation", name)
		}
		if err := auditPlan(mutate(clonePlan(clean))); err == nil {
			t.Fatalf("audit accepted the %s corruption", name)
		}
	}
}

func clonePlan(p Plan) Plan {
	out := p
	out.ShardSet = make([]Shard, len(p.ShardSet))
	for i, shard := range p.ShardSet {
		out.ShardSet[i] = shard
		out.ShardSet[i].Tests = append([]TestCost(nil), shard.Tests...)
	}
	return out
}
