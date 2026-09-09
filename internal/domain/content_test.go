// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"strings"
	"testing"
)

// Content validation against an element type's schema (DESIGN.md 5.2,
// PLAN.md M7 acceptance 5).

// storyType is the element type "cmsdb seed" writes, plus a required field and
// a repeatable one, so that every branch has something to validate against.
func storyType() *ElementType {
	return &ElementType{
		ID: 1, UID: "ET1", KeyName: "story", Name: "Story", Kind: KindStory,
		Schema: `{"fields":[
			{"name":"headline","type":"text","required":true},
			{"name":"body","type":"block"},
			{"name":"published","type":"bool"},
			{"name":"words","type":"int"},
			{"name":"embargo","type":"date"},
			{"name":"source","type":"url"},
			{"name":"tags","type":"text","repeatable":true}
		]}`,
	}
}

func TestValidateContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string

		// wantFields are the field names the refusal must name, in order. An
		// empty slice means the content is acceptable.
		wantFields []string
	}{
		{
			name:    "everything declared and well typed",
			content: `{"headline":"A Story","body":"Words.","published":true,"words":420,"embargo":"2026-03-01","source":"https://example.com","tags":["film","review"]}`,
		},
		{
			name:    "only the required field",
			content: `{"headline":"A Story"}`,
		},
		{
			name:       "the required field is missing",
			content:    `{"body":"Words."}`,
			wantFields: []string{"headline"},
		},
		{
			name:       "the required field is present and empty",
			content:    `{"headline":""}`,
			wantFields: []string{"headline"},
		},
		{
			name:       "a field the schema does not declare",
			content:    `{"headline":"A Story","boyd":"typo"}`,
			wantFields: []string{"boyd"},
		},
		{
			name:       "several undeclared fields are named in a stable order",
			content:    `{"headline":"A Story","zebra":1,"apple":2}`,
			wantFields: []string{"apple", "zebra"},
		},
		{
			name:       "a string where a number belongs",
			content:    `{"headline":"A Story","words":"lots"}`,
			wantFields: []string{"words"},
		},
		{
			name:       "a fraction where a whole number belongs",
			content:    `{"headline":"A Story","words":4.5}`,
			wantFields: []string{"words"},
		},
		{
			name:       "a string where a boolean belongs",
			content:    `{"headline":"A Story","published":"yes"}`,
			wantFields: []string{"published"},
		},
		{
			name:       "a date nobody can parse",
			content:    `{"headline":"A Story","embargo":"next Tuesday"}`,
			wantFields: []string{"embargo"},
		},
		{
			name:       "a URL that is neither absolute nor a path",
			content:    `{"headline":"A Story","source":"example.com"}`,
			wantFields: []string{"source"},
		},
		{
			name:       "a repeatable field given one value",
			content:    `{"headline":"A Story","tags":"film"}`,
			wantFields: []string{"tags"},
		},
		{
			name:       "one bad element of a repeatable field is named by index",
			content:    `{"headline":"A Story","tags":["film",7]}`,
			wantFields: []string{"tags[1]"},
		},
		{
			name:       "every problem is reported, not just the first",
			content:    `{"words":"lots","boyd":"typo"}`,
			wantFields: []string{"headline", "words", "boyd"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateContent(storyType(), tc.content)
			if len(tc.wantFields) == 0 {
				if err != nil {
					t.Fatalf("ValidateContent = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateContent accepted content that violates the schema")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("ValidateContent = %v, want it to wrap ErrInvalid (a 422 at the edge)", err)
			}

			fields, ok := FieldErrorsOf(err)
			if !ok {
				t.Fatalf("ValidateContent = %v, want a *ContentError carrying field errors", err)
			}
			got := make([]string, 0, len(fields))
			for _, f := range fields {
				got = append(got, f.Field)
				if f.Message == "" {
					t.Errorf("field %q has no message; the refusal has to say what is wrong", f.Field)
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.wantFields, ",") {
				t.Errorf("the refusal names %v, want %v", got, tc.wantFields)
			}
		})
	}
}

// TestValidateContentAcceptsAnEmptyDocument covers the case every document
// starts in: created with no content at all.
func TestValidateContentAcceptsAnEmptyDocument(t *testing.T) {
	et := &ElementType{KeyName: "note", Schema: `{"fields":[{"name":"body","type":"block"}]}`}
	for _, content := range []string{"", "{}"} {
		if err := ValidateContent(et, content); err != nil {
			t.Errorf("ValidateContent(%q) = %v, want nil", content, err)
		}
	}
}

// TestValidateContentRefusesUndeclaredFieldsUnderAnEmptySchema is the honest
// reading of an element type declaring no fields: a document of that type
// carries none.
//
// It is the reason "cmsdb seed" declares a body and a deck as of M7. An empty
// declaration accepting anything would be a schema that says one thing while
// the system does another, which is the shape invariant 6 is about.
func TestValidateContentRefusesUndeclaredFieldsUnderAnEmptySchema(t *testing.T) {
	et := &ElementType{KeyName: "empty", Schema: `{"fields":[]}`}
	if err := ValidateContent(et, "{}"); err != nil {
		t.Errorf("an empty document against an empty schema = %v, want nil", err)
	}
	err := ValidateContent(et, `{"body":"Words."}`)
	if err == nil {
		t.Fatal("an element type declaring no fields accepted a document with one")
	}
	if fields, _ := FieldErrorsOf(err); len(fields) != 1 || fields[0].Field != "body" {
		t.Errorf("the refusal names %v, want just body", fields)
	}
}

func TestParseElementSchema(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		ok     bool
	}{
		{"empty", "", true},
		{"no fields", `{"fields":[]}`, true},
		{"the seeded one", `{"fields":[{"name":"body","type":"block"},{"name":"deck","type":"text"}]}`, true},
		{"with children", `{"fields":[{"name":"body","type":"block","children":["pull-quote"]}]}`, true},
		{"a field with no name", `{"fields":[{"type":"text"}]}`, false},
		{"a field declared twice", `{"fields":[{"name":"a","type":"text"},{"name":"a","type":"text"}]}`, false},
		{"a type nobody implements", `{"fields":[{"name":"a","type":"markdown"}]}`, false},
		{"not JSON", `{`, false},
		{"an unknown key", `{"feilds":[]}`, false},
	} {
		_, err := ParseElementSchema(tc.schema)
		if tc.ok != (err == nil) {
			t.Errorf("%s: ParseElementSchema = %v, want ok=%t", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ParseElementSchema = %v, want it to wrap ErrInvalid", tc.name, err)
		}
	}
}

// TestValidateContentShapeIsSeparate is the M3 check, kept: what it refuses is
// content no schema validator could even parse, and it runs on every draft
// edit while the schema check runs only on check-in.
func TestValidateContentShapeIsSeparate(t *testing.T) {
	if err := ValidateContentShape(`{"anything":1}`); err != nil {
		t.Errorf("the shape check refused a JSON object: %v", err)
	}
	if err := ValidateContentShape(`[1,2,3]`); !errors.Is(err, ErrInvalid) {
		t.Errorf("the shape check accepted a JSON array: %v", err)
	}
	if err := ValidateContentShape(""); err != nil {
		t.Errorf("the shape check refused empty content: %v", err)
	}
}
