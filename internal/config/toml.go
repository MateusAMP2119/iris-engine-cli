package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// TOML is the result of parsing an iris.toml: the engine/connection Layer it
// contributes, plus the keys it carried that are not engine settings and were
// therefore not honored. Reporting the ignored keys (rather than dropping them
// silently) lets the caller warn that a project-manifest-shaped key in iris.toml
// had no effect -- the file is never a project manifest.
type TOML struct {
	// Layer is the settings the file explicitly set.
	Layer Layer
	// Ignored lists the keys present in the file that are not recognized engine
	// settings, in file order. They contribute nothing to Layer.
	Ignored []string
}

// tomlValueKind is the syntactic form of a parsed iris.toml value.
type tomlValueKind int

const (
	// tomlString is a double-quoted string value.
	tomlString tomlValueKind = iota
	// tomlInt is a bare base-10 integer value.
	tomlInt
)

// ParseTOML parses an iris.toml as a deliberately thin, flat key/value file:
// each non-blank, non-comment line is `key = value`, where value is a
// double-quoted string or a bare integer. Comments run from an unquoted `#` to
// end of line, and blank lines are ignored. It is strict about syntax -- table
// headers ([...]), dotted keys, missing `=`, unterminated strings, and a value
// whose type does not match its key are all errors -- so a structured file (a
// project manifest, say) does not parse as configuration.
//
// The file is limited to engine/connection settings: the recognized keys are
// socket, host, token, pg_dsn, retain, journal_partition_rows, objects_path,
// tcp, tls_cert, and tls_key. Any other well-formed key -- including the
// project-level keys of an iris-declare.yaml (name, run, reads, writes,
// depends_on, ...) -- is not honored: it is recorded in Ignored and contributes
// nothing to the resolved settings. iris.toml is never a project manifest.
func ParseTOML(data []byte) (TOML, error) {
	var out TOML
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			return TOML{}, fmt.Errorf("config: iris.toml line %d: tables are not supported (flat key = value only)", i+1)
		}
		key, rawValue, ok := strings.Cut(line, "=")
		if !ok {
			return TOML{}, fmt.Errorf("config: iris.toml line %d: missing '=' in %q", i+1, line)
		}
		key = strings.TrimSpace(key)
		if !isBareKey(key) {
			return TOML{}, fmt.Errorf("config: iris.toml line %d: malformed key %q (flat identifiers only)", i+1, key)
		}
		kind, str, num, err := parseTOMLValue(rawValue)
		if err != nil {
			return TOML{}, fmt.Errorf("config: iris.toml line %d (%s): %w", i+1, key, err)
		}
		if err := out.assign(key, kind, str, num); err != nil {
			return TOML{}, fmt.Errorf("config: iris.toml line %d: %w", i+1, err)
		}
	}
	return out, nil
}

// LoadTOMLFile reads and parses the iris.toml at path. An absent file is not an
// error: the zero-config path has no iris.toml, so a missing file contributes an
// empty layer and no ignored keys.
func LoadTOMLFile(path string) (TOML, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the resolved iris.toml location the CLI computes from the workspace, not attacker-controlled network input.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TOML{}, nil
		}
		return TOML{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	res, err := ParseTOML(data)
	if err != nil {
		return TOML{}, err
	}
	return res, nil
}

// assign routes one parsed key/value into the Layer under its expected type, or
// records the key as ignored when it is not a recognized engine setting. A value
// whose syntactic kind does not match the key's type is an error.
func (t *TOML) assign(key string, kind tomlValueKind, str string, num int64) error {
	switch key {
	case "socket":
		return setString(&t.Layer.Socket, key, kind, str)
	case "host":
		return setString(&t.Layer.Host, key, kind, str)
	case "token":
		return setString(&t.Layer.Token, key, kind, str)
	case "pg_dsn":
		return setString(&t.Layer.PgDSN, key, kind, str)
	case "objects_path":
		return setString(&t.Layer.ObjectsPath, key, kind, str)
	case "tcp":
		return setString(&t.Layer.TCP, key, kind, str)
	case "tls_cert":
		return setString(&t.Layer.TLSCert, key, kind, str)
	case "tls_key":
		return setString(&t.Layer.TLSKey, key, kind, str)
	case "retain":
		return setInt(&t.Layer.Retain, key, kind, num)
	case "journal_partition_rows":
		return setInt(&t.Layer.JournalPartitionRows, key, kind, num)
	default:
		// Not an engine/connection setting: not honored, recorded so the caller
		// can warn. iris.toml is never a project manifest.
		t.Ignored = append(t.Ignored, key)
		return nil
	}
}

// setString points dst at the parsed string, requiring the value to be a quoted
// string.
func setString(dst **string, key string, kind tomlValueKind, str string) error {
	if kind != tomlString {
		return fmt.Errorf("%s expects a quoted string", key)
	}
	v := str
	*dst = &v
	return nil
}

// setInt points dst at the parsed integer, requiring the value to be a bare
// integer.
func setInt(dst **int64, key string, kind tomlValueKind, num int64) error {
	if kind != tomlInt {
		return fmt.Errorf("%s expects an integer", key)
	}
	v := num
	*dst = &v
	return nil
}

// parseTOMLValue classifies the right-hand side of a key/value line. A value that
// begins with a double quote is a string, read up to its closing quote with any
// trailing inline comment allowed; anything else is a bare token, stripped of an
// inline comment and parsed as an integer. A bare token that is not an integer
// (e.g. true, or an unquoted path) is rejected, since the documented settings are
// only strings and integers.
func parseTOMLValue(raw string) (kind tomlValueKind, str string, num int64, err error) {
	v := strings.TrimSpace(raw)
	if strings.HasPrefix(v, "\"") {
		s, err := parseQuoted(v)
		if err != nil {
			return 0, "", 0, err
		}
		return tomlString, s, 0, nil
	}
	// Bare value: strip an inline comment, then require an integer.
	if idx := strings.IndexByte(v, '#'); idx >= 0 {
		v = strings.TrimSpace(v[:idx])
	}
	if v == "" {
		return 0, "", 0, errors.New("empty value")
	}
	n, perr := parseInt(v)
	if perr != nil {
		return 0, "", 0, fmt.Errorf("value must be a quoted string or an integer: %w", perr)
	}
	return tomlInt, "", n, nil
}

// parseQuoted reads a double-quoted string at the start of v and returns its
// contents. The closing quote is the next unescaped double quote; \" and \\ are
// the recognized escapes. Anything after the closing quote must be blank or an
// inline comment.
func parseQuoted(v string) (string, error) {
	var b strings.Builder
	i := 1 // skip the opening quote
	for i < len(v) {
		c := v[i]
		switch c {
		case '\\':
			if i+1 >= len(v) {
				return "", errors.New("unterminated string")
			}
			next := v[i+1]
			if next != '"' && next != '\\' {
				return "", fmt.Errorf("unsupported escape \\%c", next)
			}
			b.WriteByte(next)
			i += 2
		case '"':
			rest := strings.TrimSpace(v[i+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return "", fmt.Errorf("trailing content after string: %q", rest)
			}
			return b.String(), nil
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", errors.New("unterminated string")
}

// isBareKey reports whether key is a flat identifier: a non-empty run of ASCII
// letters, digits, underscores, and dashes. Dotted keys (a.b) and any other
// shape are rejected, keeping iris.toml flat.
func isBareKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
