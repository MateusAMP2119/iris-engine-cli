package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the response-side half of the read-API wire contract: the {
// "data": [...], "page": { "next_after": <key|null>, "limit": <n> } } success
// envelope, the shared error envelope with its closed code set, and the
// per-column-type JSON serialization rules. It is pure rendering: a compiled
// response shape (ordered columns with their Postgres types), served rows, and
// the request's cursor plan go in; the exact envelope bytes come out. It never
// opens a connection, touches a server, or interprets a value beyond its
// declared type -- in particular a recorded_at audit string is an opaque text
// value, emitted verbatim and never parsed or ordered by. RenderPage and
// RenderError produce those envelope bytes for a caller that needs them exactly;
// the mounted routes serve the same shapes through the mux's write helpers
// (WriteData, WriteDataPage, WriteError in api.go and endpoint.go). EncodeRow
// renders a single row on its own -- the envelope-free row form the NDJSON
// streams emit, one per line.
//
// Byte-exactness is deliberate: rows mirror the source columns in projection
// order (never a canonicalized or alphabetized object), so the renderer builds
// the JSON itself instead of round-tripping through a Go map.

// ErrorCode is a wire error code: the closed set the error envelope admits.
// Nothing outside this set is ever emitted.
type ErrorCode string

// The closed error-code set.
const (
	// CodeBadParam is a malformed, unknown, repeated, or unparseable param (400).
	CodeBadParam ErrorCode = "bad_param"
	// CodeUnauthorized is a missing or bad token on the TCP listener (401).
	CodeUnauthorized ErrorCode = "unauthorized"
	// CodeForbidden is a missing scope or grant (403).
	CodeForbidden ErrorCode = "forbidden"
	// CodeNotFound is an unknown endpoint or resource (404).
	CodeNotFound ErrorCode = "not_found"
	// CodeMethodNotAllowed is any non-GET method on the read surface (405).
	CodeMethodNotAllowed ErrorCode = "method_not_allowed"
	// CodeInternal is an engine fault (500).
	CodeInternal ErrorCode = "internal"
)

// Valid reports whether the code is in the closed set. RenderError refuses an
// out-of-set code, so an invalid code can never reach the wire.
func (c ErrorCode) Valid() bool {
	switch c {
	case CodeBadParam, CodeUnauthorized, CodeForbidden, CodeNotFound,
		CodeMethodNotAllowed, CodeInternal:
		return true
	default:
		return false
	}
}

// HTTPStatus returns the code's fixed HTTP status (the status matrix): 400
// bad_param, 401 unauthorized, 403 forbidden, 404 not_found, 405
// method_not_allowed, 500 internal. An out-of-set code maps to 500 defensively;
// RenderError refuses it before it can be served.
func (c ErrorCode) HTTPStatus() int {
	switch c {
	case CodeBadParam:
		return 400
	case CodeUnauthorized:
		return 401
	case CodeForbidden:
		return 403
	case CodeNotFound:
		return 404
	case CodeMethodNotAllowed:
		return 405
	default:
		return 500
	}
}

// ResponseColumn is one column of a route's response shape: its name and its
// Postgres type, which picks the serialization rule. A response's columns are
// ordered; rows mirror them in that order.
type ResponseColumn struct {
	// Name is the source column name, the row object's key.
	Name string
	// PgType is the column's Postgres type (the closed type mapping), which
	// selects the JSON form.
	PgType string
}

// EndpointColumns resolves a compiled /q endpoint's response shape: its
// projected fields in declaration order, each with its source column's Postgres
// type resolved through the closed type mapping. src is the endpoint's declared
// source table; a projected field missing from it is an error (the compile
// validated the projection, so a disagreement is a caller fault, not a request
// fault).
func EndpointColumns(ce *declare.CompiledEndpoint, src *declare.Table) ([]ResponseColumn, error) {
	if ce == nil {
		return nil, errors.New("api: endpoint columns: nil compiled endpoint")
	}
	if src == nil {
		return nil, errors.New("api: endpoint columns: nil source table")
	}
	cols := make(map[string]declare.Column, len(src.Columns))
	for _, c := range src.Columns {
		cols[c.Name] = c
	}
	out := make([]ResponseColumn, 0, len(ce.Fields))
	for _, f := range ce.Fields {
		c, ok := cols[f]
		if !ok {
			return nil, fmt.Errorf("api: endpoint columns: field %q is not a column of %s.%s", f, src.Schema, src.Table)
		}
		pt, err := declare.ResolveColumnType(c)
		if err != nil {
			return nil, fmt.Errorf("api: endpoint columns: %w", err)
		}
		out = append(out, ResponseColumn{Name: f, PgType: pt})
	}
	return out, nil
}

// RenderPage renders the success envelope for one served page: { "data": [
// <rows> ], "page": { "next_after": <key|null>, "limit": <n> } }. data is
// always a JSON array (empty, never null); rows mirror cols in order;
// next_after is the last served row's key when the page filled to the cursor's
// limit (nil -> null otherwise), serialized per the key column's type -- a
// composite key renders as the ordered key tuple. Every cursor key column must
// be in cols; a key outside the shape is a route misconfiguration, surfaced as
// an error, never a silent null.
func RenderPage(cols []ResponseColumn, rows []map[string]any, cursor CursorPlan) ([]byte, error) {
	typeFor := make(map[string]string, len(cols))
	for _, c := range cols {
		typeFor[c.Name] = c.PgType
	}
	for _, kc := range cursor.Key.Columns {
		if _, ok := typeFor[kc]; !ok {
			return nil, fmt.Errorf("api: render page: cursor key column %q is not in the response shape", kc)
		}
	}

	var b bytes.Buffer
	b.WriteString(`{"data":[`)
	for i, row := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		rb, err := EncodeRow(cols, row)
		if err != nil {
			return nil, err
		}
		b.Write(rb)
	}
	b.WriteString(`],"page":{"next_after":`)
	if err := writeNextAfter(&b, cursor, typeFor, rows); err != nil {
		return nil, err
	}
	b.WriteString(`,"limit":`)
	b.WriteString(strconv.Itoa(cursor.Limit))
	b.WriteString(`}}`)
	return b.Bytes(), nil
}

// writeNextAfter renders the continuation cursor: null when the page did not
// fill, the single key value serialized per its column's type, or the ordered
// key tuple for a composite key (each element per its column's type).
func writeNextAfter(b *bytes.Buffer, cursor CursorPlan, typeFor map[string]string, rows []map[string]any) error {
	na := cursor.NextAfter(rows)
	if na == nil {
		b.WriteString("null")
		return nil
	}
	if tuple, ok := na.([]any); ok {
		b.WriteByte('[')
		for i, v := range tuple {
			if i > 0 {
				b.WriteByte(',')
			}
			col := cursor.Key.Columns[i]
			frag, err := encodeValue(typeFor[col], v)
			if err != nil {
				return fmt.Errorf("api: render page: next_after key %q: %w", col, err)
			}
			b.Write(frag)
		}
		b.WriteByte(']')
		return nil
	}
	col := cursor.Key.Columns[0]
	frag, err := encodeValue(typeFor[col], na)
	if err != nil {
		return fmt.Errorf("api: render page: next_after key %q: %w", col, err)
	}
	b.Write(frag)
	return nil
}

// RenderError renders the error envelope: the same envelope reused with an
// error object, { "error": { "code": <code>, "message": <message> } }. The code
// must be in the closed set; an out-of-set code is refused here so it can never
// reach the wire.
func RenderError(code ErrorCode, message string) ([]byte, error) {
	if !code.Valid() {
		return nil, fmt.Errorf("api: render error: code %q is outside the closed set (bad_param, unauthorized, forbidden, not_found, method_not_allowed, internal)", code)
	}
	msg, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("api: render error: %w", err)
	}
	var b bytes.Buffer
	b.WriteString(`{"error":{"code":"`)
	b.WriteString(string(code))
	b.WriteString(`","message":`)
	b.Write(msg)
	b.WriteString(`}}`)
	return b.Bytes(), nil
}

// EncodeRow renders one row as a JSON object whose keys are exactly cols in
// order (rows mirror source columns), each value serialized per its column's
// Postgres type. A row missing a column is a rendering fault (an error naming
// the column), never an omitted key or a null hole. The NDJSON stream serves
// this same form, one row per line, no envelope.
func EncodeRow(cols []ResponseColumn, row map[string]any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(c.Name)
		if err != nil {
			return nil, fmt.Errorf("api: encode row: column %q: %w", c.Name, err)
		}
		b.Write(key)
		b.WriteByte(':')
		v, ok := row[c.Name]
		if !ok {
			return nil, fmt.Errorf("api: encode row: row is missing column %q", c.Name)
		}
		frag, err := encodeValue(c.PgType, v)
		if err != nil {
			return nil, fmt.Errorf("api: encode row: column %q: %w", c.Name, err)
		}
		b.Write(frag)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// encodeValue serializes one value per its column's Postgres type:
// int/bigint/smallint/double as JSON numbers, numeric as a string (no float
// round-trip loss), bool as a JSON boolean, text/varchar/uuid as strings,
// timestamptz/timestamp/date/time as RFC 3339 strings, json/jsonb inline
// (compacted, structure untouched), bytea as base64, and SQL NULL (a nil value)
// as JSON null for every type. A Go value whose type does not match its
// column's wire type is an error, never a coercion: a mismatch is a read-layer
// fault that must surface, not serialize.
func encodeValue(pgType string, v any) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	switch bt := baseType(pgType); bt {
	case "smallint", "int", "integer", "bigint":
		switch v.(type) {
		case int, int8, int16, int32, int64:
			return json.Marshal(v)
		default:
			return nil, fmt.Errorf("value %T is not an integer for column type %q", v, pgType)
		}
	case "double precision":
		switch v.(type) {
		case float32, float64:
			out, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("value for column type %q: %w", pgType, err)
			}
			return out, nil
		default:
			return nil, fmt.Errorf("value %T is not a float for column type %q", v, pgType)
		}
	case "numeric":
		s, ok := v.(string)
		if !ok {
			// A numeric is a string on the wire (no float loss); a Go float here
			// means the read layer already lost precision, so it is refused.
			return nil, fmt.Errorf("value %T is not a string for column type %q (numeric serializes as a string)", v, pgType)
		}
		return json.Marshal(s)
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("value %T is not a bool for column type %q", v, pgType)
		}
		return json.Marshal(b)
	case "text", "varchar", "uuid":
		// recorded_at audit columns are text: the string is opaque, emitted
		// verbatim, never parsed or interpreted for ordering.
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("value %T is not a string for column type %q", v, pgType)
		}
		return json.Marshal(s)
	case "timestamptz", "timestamp":
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("value %T is not a time.Time for column type %q", v, pgType)
		}
		return json.Marshal(t.Format(time.RFC3339Nano))
	case "date":
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("value %T is not a time.Time for column type %q", v, pgType)
		}
		return json.Marshal(t.Format("2006-01-02"))
	case "time":
		t, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("value %T is not a time.Time for column type %q", v, pgType)
		}
		return json.Marshal(t.Format("15:04:05.999999999"))
	case "json", "jsonb":
		raw, err := rawJSON(v)
		if err != nil {
			return nil, fmt.Errorf("value for column type %q: %w", pgType, err)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, fmt.Errorf("value for column type %q is not valid json: %w", pgType, err)
		}
		return buf.Bytes(), nil
	case "bytea":
		b, ok := v.([]byte)
		if !ok {
			return nil, fmt.Errorf("value %T is not a byte slice for column type %q", v, pgType)
		}
		return json.Marshal(base64.StdEncoding.EncodeToString(b))
	default:
		return nil, fmt.Errorf("unsupported column type %q", pgType)
	}
}

// rawJSON extracts the raw JSON bytes of a json/jsonb value: a json.RawMessage,
// a byte slice, or a string, validated before inlining so a corrupt value can
// never break the envelope's structure.
func rawJSON(v any) ([]byte, error) {
	var raw []byte
	switch j := v.(type) {
	case json.RawMessage:
		raw = j
	case []byte:
		raw = j
	case string:
		raw = []byte(j)
	default:
		return nil, fmt.Errorf("value %T is not raw json", v)
	}
	if !json.Valid(raw) {
		return nil, errors.New("value is not valid json")
	}
	return raw, nil
}
