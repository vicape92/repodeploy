package envvars

import (
	"strings"
	"testing"
)

func TestSubstituteExpandsOnlyEnvironmentAndBuildArgs(t *testing.T) {
	cfg := map[string]any{"services": map[string]any{
		"web": map[string]any{
			"image":       "${IMAGE}",
			"environment": map[string]any{"DB": "${DATABASE_URL}", "PORT": "${PORT:-3000}", "LITERAL": "cost $$5", "NUM": 3},
			"build":       map[string]any{"context": ".", "args": map[string]any{"PUBLIC": "${PUBLIC_URL}", "MODE": "${MODE:-prod}"}},
		},
		"worker": map[string]any{"environment": []any{"DB=${DATABASE_URL}", "EMPTY=${BLANK:-fallback}"}},
	}}
	vars := []Var{{Name: "DATABASE_URL", Value: "postgres://u:pa$$@db/app"}, {Name: "PUBLIC_URL", Value: "https://x", Build: true}, {Name: "BLANK", Value: ""}}
	if err := Substitute(cfg, vars); err != nil {
		t.Fatal(err)
	}
	services := cfg["services"].(map[string]any)
	web := services["web"].(map[string]any)
	env := web["environment"].(map[string]any)
	if env["DB"] != "postgres://u:pa$$$$@db/app" || env["PORT"] != "3000" || env["LITERAL"] != "cost $$5" || env["NUM"] != 3 {
		t.Fatalf("environment: %#v", env)
	}
	args := web["build"].(map[string]any)["args"].(map[string]any)
	if args["PUBLIC"] != "https://x" || args["MODE"] != "prod" {
		t.Fatalf("args: %#v", args)
	}
	if web["image"] != "${IMAGE}" {
		t.Fatal("image was expanded")
	}
	worker := services["worker"].(map[string]any)["environment"].([]any)
	if worker[0] != "DB=postgres://u:pa$$$$@db/app" || worker[1] != "EMPTY=fallback" {
		t.Fatalf("list environment: %#v", worker)
	}
}

func TestSubstituteReportsMissingNonBuildAndUnsupported(t *testing.T) {
	compose := func(env, args map[string]any) map[string]any {
		service := map[string]any{"environment": env}
		if args != nil {
			service["build"] = map[string]any{"args": args}
		}
		return map[string]any{"services": map[string]any{"app": service}}
	}
	err := Substitute(compose(map[string]any{"A": "${ZETA}", "B": "${ALPHA}"}, nil), nil)
	if err == nil || !strings.Contains(err.Error(), "undefined environment variables: ALPHA, ZETA") {
		t.Fatalf("missing: %v", err)
	}
	err = Substitute(compose(map[string]any{}, map[string]any{"S": "${SECRET}"}), []Var{{Name: "SECRET", Value: "x"}})
	if err == nil || !strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "image history") {
		t.Fatalf("non-build arg: %v", err)
	}
	for _, bad := range []string{"$NAME", "${NAME?required}", "${NAME-x}", "${NAME:-$OTHER}", "${unterminated", "${}"} {
		if err := Substitute(compose(map[string]any{"X": bad}, nil), nil); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestServiceEnvEscapesAndLimitsBuildArgs(t *testing.T) {
	env, args := ServiceEnv([]Var{{Name: "RUNTIME", Value: "a$b"}, {Name: "PUBLIC", Value: "p", Build: true}})
	if env["RUNTIME"] != "a$$b" || env["PUBLIC"] != "p" || len(args) != 1 || args["PUBLIC"] != "p" {
		t.Fatalf("env=%v args=%v", env, args)
	}
}
