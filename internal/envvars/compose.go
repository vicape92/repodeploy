package envvars

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// InterpolationHelp names the only accepted Compose interpolation.
const InterpolationHelp = "Compose interpolation is only supported as ${NAME} or ${NAME:-default} inside environment values and build args"

// Substitute expands variables in a decoded Compose file in place. Only the
// values of services.*.environment and services.*.build.args are expanded;
// every other "$" is left for the caller's validation to reject, so variables
// cannot change images, ports, volumes or privileges. Inserted values have
// "$" doubled so Compose keeps them literal.
func Substitute(cfg map[string]any, vars []Var) error {
	byName := make(map[string]Var, len(vars))
	for _, v := range vars {
		byName[v.Name] = v
	}
	r := resolver{vars: byName, missing: map[string]bool{}, notBuild: map[string]bool{}}
	services, _ := cfg["services"].(map[string]any)
	for _, raw := range services {
		service, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch env := service["environment"].(type) {
		case map[string]any:
			for key, value := range env {
				if s, ok := value.(string); ok {
					out, err := r.expand(s, false)
					if err != nil {
						return err
					}
					env[key] = out
				}
			}
		case []any:
			for i, entry := range env {
				s, ok := entry.(string)
				if !ok {
					continue
				}
				key, value, found := strings.Cut(s, "=")
				if !found {
					continue
				}
				out, err := r.expand(value, false)
				if err != nil {
					return err
				}
				env[i] = key + "=" + out
			}
		}
		if build, ok := service["build"].(map[string]any); ok {
			if args, ok := build["args"].(map[string]any); ok {
				for key, value := range args {
					if s, ok := value.(string); ok {
						out, err := r.expand(s, true)
						if err != nil {
							return err
						}
						args[key] = out
					}
				}
			}
		}
	}
	if len(r.notBuild) > 0 {
		return fmt.Errorf("build args use %s, which are not available at build time; build arguments are stored in the image history, so enable the build-time flag for them on the project page or with repodeploy env set --build", sortedNames(r.notBuild))
	}
	if len(r.missing) > 0 {
		return fmt.Errorf("undefined environment variables: %s; define them on the project page or with repodeploy env set", sortedNames(r.missing))
	}
	return nil
}

// ServiceEnv returns the environment and build args for Dockerfile mode:
// every variable at runtime, build-flagged ones also as build args.
func ServiceEnv(vars []Var) (env, args map[string]any) {
	env, args = map[string]any{}, map[string]any{}
	for _, v := range vars {
		env[v.Name] = escape(v.Value)
		if v.Build {
			args[v.Name] = escape(v.Value)
		}
	}
	return env, args
}

type resolver struct {
	vars              map[string]Var
	missing, notBuild map[string]bool
}

func (r resolver) expand(s string, build bool) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			out.WriteByte(s[i])
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "$$") {
			out.WriteString("$$")
			i += 2
			continue
		}
		end := strings.IndexByte(s[i:], '}')
		if !strings.HasPrefix(s[i:], "${") || end < 0 {
			return "", errors.New(InterpolationHelp)
		}
		expr := s[i+2 : i+end]
		name, def, hasDefault := strings.Cut(expr, ":-")
		if !ValidName(name) || strings.ContainsAny(def, "${") {
			return "", fmt.Errorf("unsupported interpolation ${%s}; %s", expr, InterpolationHelp)
		}
		v, ok := r.vars[name]
		switch {
		case ok && v.Value == "" && hasDefault:
			out.WriteString(def)
		case ok && build && !v.Build:
			r.notBuild[name] = true
		case ok:
			out.WriteString(escape(v.Value))
		case hasDefault:
			out.WriteString(def)
		default:
			r.missing[name] = true
		}
		i += end + 1
	}
	return out.String(), nil
}

func escape(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func sortedNames(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
