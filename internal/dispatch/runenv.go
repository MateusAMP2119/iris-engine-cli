package dispatch

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// This file is the run-environment resolver: the pure, leader-side computation that
// turns a pipeline's declared env map and env_file list into the deterministic
// declared overlay a run starts with (env/env_file). The overlay is what StartRun's
// composeEnv (run.go) appends after the inherited daemon environment, so os/exec
// keeps the overlay's value for any key the daemon also sets -- the declared entries
// win over the inherited environment. The resolver reads no clock and performs no I/O
// of its own: the host-env lookup and the file reader are injected, so the leader
// passes os.Getenv and os.ReadFile while a test passes fakes.
//
// Precedence, lowest to highest: inherited daemon environment (added by composeEnv,
// not here) < env_file entries (later files over earlier) < declared env entries.
// Secrets are resolved at dispatch and never stored in meta; env_file contents are
// re-read on every call, so a file edit takes effect on the next run without re-apply.

// ErrEnvFileParse reports a malformed env_file line: a non-comment, non-blank line
// that is not KEY=VALUE, or one whose key is empty. The wrapped error names the file
// and 1-based line so the operator can find the offending secret line directly.
var ErrEnvFileParse = errors.New("dispatch: env_file parse")

// ResolveRunEnv resolves a run's declared environment overlay from its declared env
// map and env_file paths. The result is a deterministic, key-sorted slice of
// KEY=VALUE strings: the env_file entries (each file read fresh through readFile,
// later files overriding earlier ones) with the declared env map layered on top, so
// an explicit env entry wins over the same key in an env_file.
//
// declaredEnv values are Compose-style: a literal passes through verbatim, and every
// ${NAME} is replaced by hostEnv("NAME"). An unset host variable resolves to the
// empty string (Compose-style), $$ is an escaped literal dollar, and any other $ is
// left literal. hostEnv is the daemon-environment lookup (os.Getenv in production);
// readFile reads an env_file's bytes (os.ReadFile in production). Both are injected so
// the resolver stays pure and a test can drive it without touching the environment or
// the filesystem; a nil hostEnv is treated as an all-empty environment.
//
// A file that cannot be read fails the resolve, wrapping the reader's error so a
// caller can still test errors.Is(err, fs.ErrNotExist) to tell a missing file from
// another read failure. A malformed env_file line fails with ErrEnvFileParse naming
// the file and line.
func ResolveRunEnv(
	declaredEnv map[string]string,
	envFilePaths []string,
	hostEnv func(string) string,
	readFile func(path string) ([]byte, error),
) ([]string, error) {
	if hostEnv == nil {
		hostEnv = func(string) string { return "" }
	}

	merged := make(map[string]string, len(declaredEnv))

	// env_file entries first (lower precedence). Each file is read fresh on every
	// call -- there is no cache -- so a change to a file is picked up next run.
	for _, path := range envFilePaths {
		data, err := readFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("dispatch: resolve run env: env_file %s does not exist: %w", path, err)
			}
			return nil, fmt.Errorf("dispatch: resolve run env: read env_file %s: %w", path, err)
		}
		entries, err := parseEnvFile(path, data)
		if err != nil {
			return nil, fmt.Errorf("dispatch: resolve run env: %w", err)
		}
		for k, v := range entries {
			merged[k] = v
		}
	}

	// Declared env entries win over env_file, resolving ${HOST_VAR} interpolations.
	for k, raw := range declaredEnv {
		merged[k] = interpolate(raw, hostEnv)
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out, nil
}

// parseEnvFile parses an env_file's bytes into a KEY=VALUE map. Blank lines and lines
// whose first non-space rune is '#' are ignored; every other line must contain a '='
// with a non-empty key, or the parse fails with ErrEnvFileParse naming the file and
// 1-based line. The key and value are trimmed of surrounding whitespace; the value is
// otherwise verbatim (no quote unwrapping and no interpolation -- env_file secrets are
// literal). A later line for a repeated key overrides an earlier one.
func parseEnvFile(path string, data []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq < 0 {
			return nil, fmt.Errorf("%w: %s:%d: not KEY=VALUE (no '=')", ErrEnvFileParse, path, i+1)
		}
		key := strings.TrimSpace(trimmed[:eq])
		if key == "" {
			return nil, fmt.Errorf("%w: %s:%d: empty key", ErrEnvFileParse, path, i+1)
		}
		out[key] = strings.TrimSpace(trimmed[eq+1:])
	}
	return out, nil
}

// interpolate resolves a declared env value's ${NAME} references through hostEnv,
// Compose-style: ${NAME} becomes hostEnv("NAME") (empty when unset), $$ is a literal
// dollar, and any other $ (a bare $, or ${ with no closing brace) is left verbatim.
func interpolate(value string, hostEnv func(string) string) string {
	if !strings.ContainsRune(value, '$') {
		return value
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); {
		c := value[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		// c == '$': look at the next byte to classify the sequence.
		if i+1 >= len(value) {
			b.WriteByte('$') // trailing lone '$'
			i++
			continue
		}
		switch next := value[i+1]; next {
		case '$':
			b.WriteByte('$') // escaped: $$ -> $
			i += 2
		case '{':
			end := strings.IndexByte(value[i+2:], '}')
			if end < 0 {
				// No closing brace: leave the '${' literal and continue past it.
				b.WriteString("${")
				i += 2
				continue
			}
			name := value[i+2 : i+2+end]
			if name == "" {
				b.WriteString("${}") // empty ref is not a variable; keep literal
			} else {
				b.WriteString(hostEnv(name))
			}
			i += 2 + end + 1
		default:
			b.WriteByte('$') // bare '$' not part of an escape or a ${...} ref
			i++
		}
	}
	return b.String()
}
