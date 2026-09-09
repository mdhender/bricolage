// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/mdhender/bricolage/internal/domain"
)

// The editor is drawn from the element type's schema, and a form is turned
// back into the JSON content tree the same way.
//
// This is marshalling and not business logic: the shape it produces is checked
// by domain.ValidateContent on check-in, which is the one place content is
// validated (DESIGN.md 5.2), and nothing here decides whether a value is
// acceptable. A field the schema does not declare is not offered, because a
// check-in would refuse it by name and the person who typed it would have no
// way to find the box it came from.

// fieldInput is one box on the editor form.
type fieldInput struct {
	Def domain.FieldDef

	// Value is a scalar field's value, already decoded from JSON.
	Value string

	// Lines is a repeatable field's values, one per line. A textarea is the
	// honest control for "as many as you like" without JavaScript, and the
	// line is the separator a person can see.
	Lines string

	// Raw is the value as it stands when it is not the shape the schema
	// declares -- an array in a scalar field, an object anywhere. A working
	// draft may hold anything until it is checked in, so the editor shows
	// what is there rather than silently dropping it.
	Raw string
}

// Control names the input to draw: "text", "block", "int", "bool", "date",
// "url", "document", or "lines" for anything repeatable.
func (f fieldInput) Control() string {
	if f.Def.Repeatable {
		return "lines"
	}
	return f.Def.Type
}

// editorFields builds the form from a schema and the draft's content.
func editorFields(schema domain.ElementSchema, content string) []fieldInput {
	tree := map[string]json.RawMessage{}
	if strings.TrimSpace(content) != "" {
		_ = json.Unmarshal([]byte(content), &tree)
	}

	out := make([]fieldInput, 0, len(schema.Fields))
	for _, def := range schema.Fields {
		in := fieldInput{Def: def}
		raw, present := tree[def.Name]
		if !present {
			out = append(out, in)
			continue
		}
		if def.Repeatable {
			var values []json.RawMessage
			if err := json.Unmarshal(raw, &values); err != nil {
				in.Raw = string(raw)
			} else {
				lines := make([]string, 0, len(values))
				for _, v := range values {
					lines = append(lines, scalarString(v))
				}
				in.Lines = strings.Join(lines, "\n")
			}
			out = append(out, in)
			continue
		}
		in.Value = scalarString(raw)
		out = append(out, in)
	}
	return out
}

// contentFromForm rebuilds the JSON content tree from what was submitted.
//
// An empty box writes no key at all rather than an empty value: clearing a
// field is removing it, and a required field left empty is refused on
// check-in, by name, which is where that refusal belongs.
func contentFromForm(schema domain.ElementSchema, r *http.Request) (string, error) {
	tree := map[string]any{}
	for _, def := range schema.Fields {
		name := "field." + def.Name
		if def.Repeatable {
			raw := strings.TrimSpace(r.PostFormValue(name))
			if raw == "" {
				continue
			}
			var values []any
			for _, line := range strings.Split(raw, "\n") {
				line = strings.TrimRight(line, "\r")
				if strings.TrimSpace(line) == "" {
					continue
				}
				v, err := typedValue(def, line)
				if err != nil {
					return "", err
				}
				values = append(values, v)
			}
			tree[def.Name] = values
			continue
		}

		raw := r.PostFormValue(name)
		if def.Type != domain.FieldBlock {
			raw = strings.TrimSpace(raw)
		}
		if raw == "" {
			continue
		}
		v, err := typedValue(def, raw)
		if err != nil {
			return "", err
		}
		tree[def.Name] = v
	}

	body, err := json.Marshal(tree)
	if err != nil {
		return "", fmt.Errorf("content: %v: %w", err, domain.ErrInvalid)
	}
	return string(body), nil
}

// typedValue converts one typed box into the JSON value the schema declares.
//
// A number that is not a number, or a boolean that is neither, is refused here
// rather than written and refused on check-in: the person is looking at the
// box they typed it in.
func typedValue(def domain.FieldDef, raw string) (any, error) {
	switch def.Type {
	case domain.FieldInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a whole number: %w", def.Name, raw, domain.ErrInvalid)
		}
		return n, nil
	case domain.FieldBool:
		switch strings.ToLower(raw) {
		case "true", "yes", "on", "1":
			return true, nil
		case "false", "no", "off", "0":
			return false, nil
		default:
			return nil, fmt.Errorf("%s: %q is not true or false: %w", def.Name, raw, domain.ErrInvalid)
		}
	default:
		return raw, nil
	}
}

// scalarString renders one JSON value as the text an input holds. Anything
// that is not a string, a number, or a boolean is shown as the JSON it is.
func scalarString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return strings.TrimSpace(string(raw))
}

// contentPair is one field as the document page shows it.
type contentPair struct {
	Name  string
	Value string
}

// contentPairs renders a version's content for reading, in a stable order:
// the schema's, then anything else the draft happens to carry, sorted.
func contentPairs(schema domain.ElementSchema, content string) []contentPair {
	tree := map[string]json.RawMessage{}
	if strings.TrimSpace(content) != "" {
		if err := json.Unmarshal([]byte(content), &tree); err != nil {
			return []contentPair{{Name: "content", Value: content}}
		}
	}

	seen := map[string]bool{}
	out := make([]contentPair, 0, len(tree))
	for _, def := range schema.Fields {
		raw, ok := tree[def.Name]
		if !ok {
			continue
		}
		seen[def.Name] = true
		out = append(out, contentPair{Name: def.Name, Value: displayValue(raw)})
	}

	var rest []string
	for name := range tree {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, contentPair{Name: name, Value: displayValue(tree[name])})
	}
	return out
}

// displayValue renders one value, joining an array with newlines.
func displayValue(raw json.RawMessage) string {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err == nil {
		parts := make([]string, 0, len(values))
		for _, v := range values {
			parts = append(parts, scalarString(v))
		}
		return strings.Join(parts, "\n")
	}
	return scalarString(raw)
}
