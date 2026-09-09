// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Content validation against an element type's schema (DESIGN.md 5.2,
// PLAN.md M7 acceptance 5).
//
// Eighteen entity-attribute-value tables in the system we learned from become
// one JSON column here, and the price of that is that the database can no
// longer say which fields a document carries. This is the function that says
// it instead, and it is pure: a table of field definitions, a tree of content,
// and a list of what is wrong with it.
//
// It runs on check-in and not on every keystroke. A working draft may be
// invalid -- refusing a half-finished paragraph is how a writer loses a
// sentence -- and a checked-in version may not, because a checked-in version
// is what a publish pins and a renderer walks.

// The field types an element type may declare.
//
// The set is small on purpose. Every one of them is a shape the renderer can
// be written against and the editor can draw a control for; a type nobody can
// render is a column of prose with a label on it, which is what "text" is
// already for.
const (
	FieldText  = "text"  // a single line
	FieldBlock = "block" // a paragraph or more, possibly with markup
	FieldInt   = "int"
	FieldBool  = "bool"
	FieldDate  = "date" // a date this system can parse
	FieldURL   = "url"

	// FieldDocument is a reference to another document, written as that
	// document's uid (invariant 10). It is the field the related-asset
	// cascade reads: publishing a document publishes the documents its
	// content names, and the only way a traversal can know what they are is
	// for the schema to declare which fields hold one (DESIGN.md 8.2,
	// PLAN.md M10).
	//
	// It is a declared type rather than a convention over "url" or a scan of
	// the whole content tree for anything uid-shaped. A cascade that guessed
	// would publish a document because somebody quoted a uid in a paragraph,
	// and one that scanned only "url" fields could not tell an internal
	// reference from a link to somebody else's site.
	FieldDocument = "document"
)

// FieldTypes are the declarable types, in a stable order for the CLI and the
// admin screens.
var FieldTypes = []string{FieldText, FieldBlock, FieldInt, FieldBool, FieldDate, FieldURL, FieldDocument}

// ValidFieldType reports whether s is one of them.
func ValidFieldType(s string) bool {
	for _, t := range FieldTypes {
		if t == s {
			return true
		}
	}
	return false
}

// FieldDef is one field an element type declares (DESIGN.md 5.2).
type FieldDef struct {
	Name string `json:"name"`
	Type string `json:"type"`

	// Required means the content must carry a non-empty value. A repeatable
	// required field needs at least one element.
	Required bool `json:"required,omitempty"`

	// Repeatable means the content carries a JSON array of values rather than
	// one value.
	Repeatable bool `json:"repeatable,omitempty"`

	// Children are the key names of the element types this field may contain.
	// They are declared and stored from M7 and enforced when M8's renderer
	// has a nested element tree to walk; until then a document's content is
	// flat, and a list of permitted children with nothing to permit is
	// recorded rather than checked.
	Children []string `json:"children,omitempty"`
}

// ElementSchema is the JSON document element_types.schema holds.
type ElementSchema struct {
	Fields []FieldDef `json:"fields"`
}

// ParseElementSchema reads an element type's schema.
//
// An empty schema is the one "cmsdb seed" writes and is valid: it declares no
// fields, so it accepts no fields, which is an honest starting point rather
// than a set of fields nobody asked for.
func ParseElementSchema(s string) (ElementSchema, error) {
	var out ElementSchema
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return ElementSchema{}, fmt.Errorf("element type schema: %v: %w", err, ErrInvalid)
	}
	if dec.More() {
		return ElementSchema{}, fmt.Errorf("element type schema: more than one JSON value: %w", ErrInvalid)
	}
	return out, out.Validate()
}

// Validate reports whether the schema itself is well formed.
//
// It is checked when an element type is written rather than when a document is
// checked in against it. A schema declaring a type nobody implements would
// otherwise refuse every document of that type with a message about the
// document, which sends the wrong person looking.
func (s ElementSchema) Validate() error {
	seen := map[string]bool{}
	for i, f := range s.Fields {
		switch {
		case strings.TrimSpace(f.Name) == "":
			return fmt.Errorf("element type schema: field %d has no name: %w", i, ErrInvalid)
		case seen[f.Name]:
			return fmt.Errorf("element type schema: field %q is declared twice: %w", f.Name, ErrInvalid)
		case !ValidFieldType(f.Type):
			return fmt.Errorf("element type schema: field %q has type %q, not one of %s: %w",
				f.Name, f.Type, strings.Join(FieldTypes, ", "), ErrInvalid)
		}
		seen[f.Name] = true
	}
	return nil
}

// Field returns the definition of one field.
func (s ElementSchema) Field(name string) (FieldDef, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return FieldDef{}, false
}

// FieldError is one thing wrong with a piece of content: which field, and
// what.
//
// It carries the field name rather than a code because the person reading it
// is the person who typed the content, and because PLAN.md M7 acceptance 5
// asks for the offending fields to be named.
type FieldError struct {
	Field   string
	Message string
}

func (e FieldError) String() string { return e.Field + ": " + e.Message }

// ContentError reports content that does not validate against its element
// type's schema.
//
// It carries every field error rather than the first, because a person fixing
// a document wants the list and not a sequence of round trips. It answers to
// ErrInvalid, so the transport edge maps it to 422 through the one mapping
// function it has (DESIGN.md 14), and internal/api reads Fields to fill in the
// problem document's "errors" member.
type ContentError struct {
	// KeyName is the element type the content was validated against.
	KeyName string

	Fields []FieldError
}

func (e *ContentError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.String())
	}
	return fmt.Sprintf("content does not match element type %q: %s", e.KeyName, strings.Join(parts, "; "))
}

// Is makes a content failure answer to ErrInvalid.
func (e *ContentError) Is(target error) bool { return target == ErrInvalid }

// FieldErrorsOf returns the field errors carried by err, and whether err was a
// content failure at all.
//
// It is how the transport edge fills in the problem document's "errors" member
// without knowing anything else about the error, and it is the same shape as
// GuardOf (DESIGN.md 12).
func FieldErrorsOf(err error) ([]FieldError, bool) {
	var ce *ContentError
	if errors.As(err, &ce) {
		return ce.Fields, true
	}
	return nil, false
}

// ValidateContent reports what is wrong with content under et's schema
// (DESIGN.md 5.2).
//
// It returns nil when the content is acceptable and a *ContentError naming
// every offending field otherwise. Three things are checked, and the third is
// the one that earns its keep:
//
//   - a required field must be present and non-empty;
//   - a value must have the declared type, and a repeatable field must be an
//     array of them;
//   - a field the schema does not declare is refused.
//
// The last one is refused rather than ignored for the reason unknown JSON
// fields are refused at the transport edge: content carrying "boyd" alongside
// "body" is a mistake, and accepting it silently means it is discovered later,
// by somebody else, as an empty page.
func ValidateContent(et *ElementType, content string) error {
	if et == nil {
		return fmt.Errorf("content: no element type to validate against: %w", ErrInvalid)
	}
	schema, err := ParseElementSchema(et.Schema)
	if err != nil {
		return err
	}
	if err := ValidateContentShape(content); err != nil {
		return err
	}

	var tree map[string]json.RawMessage
	if strings.TrimSpace(content) != "" {
		if err := json.Unmarshal([]byte(content), &tree); err != nil {
			return fmt.Errorf("content: not a JSON object: %v: %w", err, ErrInvalid)
		}
	}

	var problems []FieldError
	for _, f := range schema.Fields {
		raw, present := tree[f.Name]
		if !present {
			if f.Required {
				problems = append(problems, FieldError{Field: f.Name, Message: "is required and is missing"})
			}
			continue
		}
		problems = append(problems, checkField(f, raw)...)
	}

	// Anything the schema does not declare, reported in a stable order: a map
	// iterates differently every run, and a 422 whose body changes shape
	// between two identical requests is a 422 nobody can write a test for.
	var unknown []string
	for name := range tree {
		if _, ok := schema.Field(name); !ok {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		problems = append(problems, FieldError{
			Field:   name,
			Message: fmt.Sprintf("element type %q declares no such field", et.KeyName),
		})
	}

	if len(problems) == 0 {
		return nil
	}
	return &ContentError{KeyName: et.KeyName, Fields: problems}
}

// checkField validates one declared field's value.
func checkField(f FieldDef, raw json.RawMessage) []FieldError {
	if f.Repeatable {
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return []FieldError{{Field: f.Name, Message: "is repeatable, so it must be a JSON array"}}
		}
		if f.Required && len(values) == 0 {
			return []FieldError{{Field: f.Name, Message: "is required and is empty"}}
		}
		var out []FieldError
		for i, v := range values {
			if msg, ok := checkValue(f.Type, v); !ok {
				out = append(out, FieldError{
					Field:   fmt.Sprintf("%s[%d]", f.Name, i),
					Message: msg,
				})
			}
		}
		return out
	}

	if msg, ok := checkValue(f.Type, raw); !ok {
		return []FieldError{{Field: f.Name, Message: msg}}
	}
	if f.Required && isEmptyValue(raw) {
		return []FieldError{{Field: f.Name, Message: "is required and is empty"}}
	}
	return nil
}

// checkValue reports whether one JSON value has the declared type, and what to
// say when it does not.
func checkValue(fieldType string, raw json.RawMessage) (string, bool) {
	switch fieldType {
	case FieldText, FieldBlock:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "must be a string", false
		}
	case FieldInt:
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return "must be a number", false
		}
		if strings.ContainsAny(n.String(), ".eE") {
			return "must be a whole number", false
		}
	case FieldBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return "must be true or false", false
		}
	case FieldDate:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "must be a date written as a string", false
		}
		if s != "" {
			if _, err := ParseCoverDate(s); err != nil {
				return "must be a date this system recognises, such as 2026-03-01", false
			}
		}
	case FieldURL:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "must be a URL written as a string", false
		}
		if s != "" && !strings.Contains(s, "://") && !strings.HasPrefix(s, "/") {
			return "must be an absolute URL or a path beginning with /", false
		}
	case FieldDocument:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "must be a document uid written as a string", false
		}
		// Whether a document by that uid exists is not asked here. domain
		// performs no I/O (invariant 1), and the answer belongs to the
		// moment of the publish rather than to the moment of the check-in:
		// a related story deleted after this version was written is a
		// refusal the cascade names by uid (PLAN.md M10 acceptance 2), not
		// a version that retroactively becomes invalid. What is refused
		// here is a value no lookup could ever be made from.
		if strings.TrimSpace(s) != s || strings.ContainsAny(s, " \t\n/") {
			return "must be a document uid, with no spaces or slashes in it", false
		}
	}
	return "", true
}

// isEmptyValue reports whether a value is the empty one for its JSON shape,
// which is what "required" is about: a field present and set to "" is not a
// field somebody filled in.
func isEmptyValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == `""` || trimmed == "null"
}
