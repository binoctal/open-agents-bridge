// Package compose is the hosting-agent half of the hosted-compose-stacks
// compose rule table (tasks 5.1/5.2). The API validated the file at source
// complete with its JS yaml parser (apps/api/src/services/compose-policy.ts);
// this package re-validates the SAME table with yaml.v3 at materialization
// -- the two yaml dialects can disagree on edge-case documents, and the node,
// not the API, is where dangerous constructs actually execute. On success it
// applies the deployment rewrite Coolify's dockercompose build pack needs:
//
//   - active services: profiles key stripped (unconditional activation --
//     Coolify runs plain `docker compose up -d` with no --profile flags)
//   - active services: deploy.resources.limits overwritten with the spec's
//     post-min cpus/memory (authoritative -- billing-tier mins applied)
//   - ingress service: ports replaced with ["127.0.0.1:hostPort:containerPort"]
//   - non-ingress active services: ports key dropped (they still reach the
//     ingress over the compose network)
//   - services outside the activation set: left untouched -- still
//     profile-gated, so they never start
//
// The rewrite is a pure function of (bytes, spec): the materialized commit
// stays deterministic for a fixed manifest.
package compose

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"gopkg.in/yaml.v3"
)

// The rule table below mirrors compose-policy.ts literal-for-literal. Keep
// both sides in lockstep with the spec deltas.

var topKeys = map[string]bool{
	"version": true, "name": true, "services": true, "networks": true, "volumes": true,
}

var serviceKeys = map[string]bool{
	"image": true, "build": true, "environment": true, "env_file": true,
	"ports": true, "expose": true, "depends_on": true, "healthcheck": true,
	"command": true, "entrypoint": true, "restart": true, "profiles": true,
	"working_dir": true, "volumes": true, "networks": true, "labels": true,
	"user": true, "init": true, "deploy": true, "tty": true, "stdin_open": true,
	"pull_policy": true, "shm_size": true, "stop_grace_period": true,
}

// deniedServiceKeys are dangerous (not merely unsupported); keep the reason
// fragments identical so operator-facing text stays greppable across sides.
var deniedServiceKeys = map[string]string{
	"privileged":    "privileged mode",
	"cap_add":       "Linux capability grants",
	"cap_drop":      "Linux capability drops",
	"sysctls":       "sysctl overrides",
	"network_mode":  "network_mode (incl. host networking)",
	"pid":           "PID namespace sharing",
	"ipc":           "IPC namespace sharing",
	"devices":       "device passthrough",
	"gpus":          "GPU reservation",
	"runtime":       "runtime override (e.g. nvidia)",
	"cgroup_parent": "cgroup parent override",
	"userns":        "user namespace override",
	"uts":           "UTS namespace override",
	"security_opt":  "security options",
}

const maxNamedVolumes = 8

// windowsDrive matches the TS `^[A-Za-z]:` host-bind prefix (e.g. "C:\data").
var windowsDrive = regexp.MustCompile(`^[A-Za-z]:`)

// Rewrite re-validates src against the rule table and applies the deployment
// rewrite. A violation is an error, never a best-effort rewrite: the R2 tree
// and the validated document must be the same thing the node executes.
func Rewrite(src []byte, spec api.ComposeSpec) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("compose %s: yaml parse failed: %w", spec.ComposePath, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("compose %s: empty document", spec.ComposePath)
	}
	// yaml aliasing can decode two keys to the SAME map value; the rewrite
	// mutates per-service state, so detach first or an anchored config would
	// cross-contaminate its alias sites.
	doc = detach(doc, map[uintptr]any{}).(map[string]any)

	active, violations := validate(doc, spec)
	if len(violations) > 0 {
		return nil, fmt.Errorf("compose %s: re-validation failed: %s",
			spec.ComposePath, strings.Join(violations, "; "))
	}

	services := active
	for _, lim := range spec.Services {
		svc := services[lim.Name]
		delete(svc, "profiles")
		if lim.Name == spec.IngressService {
			svc["ports"] = []any{fmt.Sprintf("127.0.0.1:%d:%d", spec.HostPort, spec.IngressPort)}
		} else {
			delete(svc, "ports")
		}
		// Inject the authoritative limits, preserving any sibling limit keys
		// the original declared (the API whitelist allows them).
		deploy, _ := svc["deploy"].(map[string]any)
		if deploy == nil {
			deploy = map[string]any{}
			svc["deploy"] = deploy
		}
		resources, _ := deploy["resources"].(map[string]any)
		if resources == nil {
			resources = map[string]any{}
			deploy["resources"] = resources
		}
		limits, _ := resources["limits"].(map[string]any)
		if limits == nil {
			limits = map[string]any{}
			resources["limits"] = limits
		}
		limits["cpus"] = lim.CPUs
		// Plain integer = bytes per the compose spec: unit-free sidesteps the
		// decimal/IEC ambiguity of "m"/"M" suffixes across compose versions.
		limits["memory"] = int64(lim.MemoryMiB) * 1024 * 1024
	}

	// yaml.Marshal sorts map keys, so the output is deterministic.
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("compose %s: re-marshal failed: %w", spec.ComposePath, err)
	}
	return out, nil
}

// validate re-checks the API's rule table and returns the active services'
// maps (name -> service map) plus the violation list. Admission-side checks
// (memory parsing, tier sizing, port ranges, env_file presence) are NOT
// duplicated: those decided the spec, and the rewrite overwrites the values
// they picked at. Everything that would let a container escape its sandbox
// or surprise the node IS re-checked.
func validate(doc map[string]any, spec api.ComposeSpec) (map[string]map[string]any, []string) {
	var violations []string
	push := func(format string, args ...any) {
		violations = append(violations, fmt.Sprintf(format, args...))
	}

	for key := range doc {
		if !topKeys[key] {
			push(`unsupported top-level key "%s"`, key)
		}
	}

	servicesRaw, _ := doc["services"].(map[string]any)
	if len(servicesRaw) == 0 {
		push("compose file declares no services")
		return nil, violations
	}

	// Re-derive the activation set from the document (profiles x
	// activateProfiles, same formula as the platform) and cross-check it
	// against the spec's active set: a mismatch means the file no longer
	// matches what was validated -- never deploy on guesswork.
	derived := map[string]bool{}
	for name, raw := range servicesRaw {
		svc, ok := raw.(map[string]any)
		if !ok {
			push(`service "%s" must be a mapping`, name)
			continue
		}
		if serviceActive(svc, spec.ActivateProfiles) {
			derived[name] = true
		}
	}
	specNames := map[string]bool{}
	for _, lim := range spec.Services {
		specNames[lim.Name] = true
	}
	for name := range specNames {
		if !derived[name] {
			push(`activation drift: service "%s" is in the spec but not activated by the file`, name)
		}
	}
	for name := range derived {
		if !specNames[name] {
			push(`activation drift: service "%s" is activated by the file but not in the spec`, name)
		}
	}
	if !specNames[spec.IngressService] {
		push(`ingress service "%s" is not an active service`, spec.IngressService)
	}

	// Top-level volumes: mapping shape, local driver only, no driver_opts,
	// no external.
	var declaredVolumes []string
	if raw, ok := doc["volumes"]; ok {
		vols, isMap := raw.(map[string]any)
		if !isMap {
			push("top-level volumes must be a mapping")
		} else {
			for name, cfg := range vols {
				declaredVolumes = append(declaredVolumes, name)
				m, _ := cfg.(map[string]any)
				if m == nil {
					continue
				}
				if d, ok := m["driver"]; ok && d != "local" {
					push(`volume "%s": driver "%v" is not allowed (local only)`, name, d)
				}
				if _, ok := m["driver_opts"]; ok {
					push(`volume "%s": driver_opts are not allowed`, name)
				}
				if ext, ok := m["external"]; ok && !isFalse(ext) {
					push(`volume "%s": external volumes are not allowed`, name)
				}
			}
		}
	}
	if len(declaredVolumes) > maxNamedVolumes {
		push("stack exceeds %d named volumes", maxNamedVolumes)
	}
	sort.Strings(declaredVolumes)
	declared := func(name string) bool {
		i := sort.SearchStrings(declaredVolumes, name)
		return i < len(declaredVolumes) && declaredVolumes[i] == name
	}

	if raw, ok := doc["networks"]; ok {
		nets, isMap := raw.(map[string]any)
		if !isMap {
			push("top-level networks must be a mapping")
		} else {
			for name, cfg := range nets {
				m, _ := cfg.(map[string]any)
				if m == nil {
					continue
				}
				if d, ok := m["driver"]; ok && d != "bridge" {
					push(`network "%s": driver "%v" is not allowed (bridge only)`, name, d)
				}
				if ext, ok := m["external"]; ok && !isFalse(ext) {
					push(`network "%s": external networks are not allowed`, name)
				}
			}
		}
	}

	active := map[string]map[string]any{}
	for _, lim := range spec.Services {
		raw, ok := servicesRaw[lim.Name].(map[string]any)
		if !ok {
			push(`active service "%s" missing from compose file`, lim.Name)
			continue
		}
		active[lim.Name] = raw
	}

	for name, svc := range active {
		for key := range svc {
			if reason, denied := deniedServiceKeys[key]; denied {
				push(`service "%s": %s is not allowed`, name, reason)
			} else if !serviceKeys[key] {
				push(`service "%s": unsupported service key "%s"`, name, key)
			}
		}

		if deploy, ok := svc["deploy"].(map[string]any); ok && deploy != nil {
			for key := range deploy {
				if key != "resources" {
					push(`service "%s": only deploy.resources.limits is supported (deploy.%s)`, name, key)
				}
			}
			if resources, ok := deploy["resources"].(map[string]any); ok && resources != nil {
				for key, val := range resources {
					if key == "limits" {
						continue
					}
					if key == "reservations" {
						if m, ok := val.(map[string]any); ok {
							if _, has := m["devices"]; has {
								push(`service "%s": device (GPU) reservations are not allowed`, name)
								continue
							}
						}
					}
					push(`service "%s": only deploy.resources.limits is supported (deploy.resources.%s)`, name, key)
				}
			}
		}

		if rawMounts, ok := svc["volumes"]; ok {
			mounts, _ := rawMounts.([]any)
			for _, raw := range mounts {
				switch mount := raw.(type) {
				case string:
					parts := strings.Split(mount, ":")
					switch {
					case len(parts) == 1:
						push(`service "%s": anonymous volumes are not allowed; declare a named volume`, name)
					case strings.HasPrefix(parts[0], "/") || strings.HasPrefix(parts[0], "./") ||
						strings.HasPrefix(parts[0], "../") || windowsDrive.MatchString(parts[0]):
						push(`service "%s": host path bind mounts are not allowed`, name)
					case !declared(parts[0]):
						push(`service "%s": volume "%s" is not declared in top-level volumes`, name, parts[0])
					}
				case map[string]any:
					if mount["type"] == "bind" {
						push(`service "%s": host path bind mounts are not allowed`, name)
					} else if mount["type"] == "tmpfs" {
						push(`service "%s": tmpfs mounts are not allowed`, name)
					} else if src, ok := mount["source"].(string); ok && src != "" && !declared(src) {
						push(`service "%s": volume "%s" is not declared in top-level volumes`, name, src)
					}
				}
			}
		}
	}

	return active, violations
}

// serviceActive is the platform's activation formula: no profiles key (or an
// empty/unrecognized one) means unconditional; otherwise the service is
// active iff any of its profiles is in the activation set.
func serviceActive(svc map[string]any, activate []string) bool {
	raw, ok := svc["profiles"].([]any)
	if !ok || len(raw) == 0 {
		return true
	}
	for _, p := range raw {
		s, ok := p.(string)
		if !ok {
			continue
		}
		for _, a := range activate {
			if s == a {
				return true
			}
		}
	}
	return false
}

// isFalse mirrors the TS truthiness checks around `external`: only an explicit
// false (or absent) clears the flag.
func isFalse(v any) bool {
	b, ok := v.(bool)
	return ok && !b
}

// detach deep-copies a decoded yaml value, breaking the pointer sharing alias
// resolution can introduce: every map/slice becomes a fresh container, and a
// container visited twice yields two independent copies. Without this, an
// anchored service config (`worker: *web`) would alias one map and the
// per-service rewrite would apply whichever limit landed last to BOTH.
func detach(v any, seen map[uintptr]any) any {
	switch t := v.(type) {
	case map[string]any:
		ptr := reflect.ValueOf(t).Pointer()
		if copy, ok := seen[ptr]; ok {
			return copy
		}
		copy := make(map[string]any, len(t))
		seen[ptr] = copy
		for k, item := range t {
			copy[k] = detach(item, seen)
		}
		return copy
	case []any:
		copy := make([]any, len(t))
		for i, item := range t {
			copy[i] = detach(item, seen)
		}
		return copy
	default:
		return v
	}
}
