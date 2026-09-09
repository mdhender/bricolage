// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The related-asset cascade's pure half (DESIGN.md 8.2, PLAN.md M10).
//
// Two things live in domain and both are tested exhaustively here, because
// domain is pure and there is no excuse (AGENTS.md, "Testing"): how a version's
// content says what it references, and how a refusal says who and why.

const relatedSchema = `{"fields":[
	{"name":"body","type":"block"},
	{"name":"lead","type":"document"},
	{"name":"related","type":"document","repeatable":true}
]}`

func relatedElementType(schema string) *ElementType {
	return &ElementType{KeyName: "story", Kind: KindStory, Schema: schema}
}

func TestReferences(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		content string
		want    []string
	}{
		{
			name:    "no document field declared is no references, whatever the content says",
			schema:  `{"fields":[{"name":"body","type":"block"}]}`,
			content: `{"body":"see 01JB","related":["01JB"]}`,
			want:    nil,
		},
		{
			name:    "a single document field",
			schema:  relatedSchema,
			content: `{"body":"words","lead":"01AAA"}`,
			want:    []string{"01AAA"},
		},
		{
			name:    "a repeatable one, in the order the content lists them",
			schema:  relatedSchema,
			content: `{"related":["01CCC","01AAA","01BBB"]}`,
			want:    []string{"01CCC", "01AAA", "01BBB"},
		},
		{
			name:    "the declaration order decides, then the array order",
			schema:  relatedSchema,
			content: `{"related":["01CCC"],"lead":"01AAA"}`,
			want:    []string{"01AAA", "01CCC"},
		},
		{
			name:    "a uid named twice is one reference",
			schema:  relatedSchema,
			content: `{"lead":"01AAA","related":["01AAA","01BBB","01AAA"]}`,
			want:    []string{"01AAA", "01BBB"},
		},
		{
			name:    "an empty value is not a reference",
			schema:  relatedSchema,
			content: `{"lead":"","related":["01AAA","  "]}`,
			want:    []string{"01AAA"},
		},
		{
			name:    "a field the content omits",
			schema:  relatedSchema,
			content: `{"body":"words"}`,
			want:    nil,
		},
		{
			name:    "empty content",
			schema:  relatedSchema,
			content: "",
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := References(relatedElementType(tc.schema), tc.content)
			if err != nil {
				t.Fatalf("References: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("References = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReferencesIsDeterministic is what PLAN.md M10 acceptance 6 rests on: a
// dry run and a real publish are compared, and two runs that disagreed about
// the order of the references would produce two sets nobody could compare.
//
// A map iterates differently every run, so the assertion is that many runs
// over the same content agree, not that one run produces something plausible.
func TestReferencesIsDeterministic(t *testing.T) {
	const content = `{"lead":"01AAA","related":["01EEE","01BBB","01DDD","01CCC"]}`
	want, err := References(relatedElementType(relatedSchema), content)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	for i := range 50 {
		got, err := References(relatedElementType(relatedSchema), content)
		if err != nil {
			t.Fatalf("References: %v", err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d produced %v, want %v", i, got, want)
		}
	}
}

func TestReferencesRefusesWhatItCannotRead(t *testing.T) {
	tests := []struct {
		name    string
		et      *ElementType
		content string
	}{
		{"no element type", nil, `{}`},
		{"content that is not an object", relatedElementType(relatedSchema), `["01AAA"]`},
		{"a repeatable field holding one value", relatedElementType(relatedSchema), `{"related":"01AAA"}`},
		{"a single field holding an array", relatedElementType(relatedSchema), `{"lead":["01AAA"]}`},
		{"a schema that does not parse", relatedElementType(`{`), `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := References(tc.et, tc.content); !errors.Is(err, ErrInvalid) {
				t.Errorf("References = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestDocumentFieldValidates covers the new field type on the check-in path
// (DESIGN.md 5.2). What is refused is a value no lookup could ever be made
// from; whether a document by that uid exists is the cascade's question and
// not this one's, because domain performs no I/O (invariant 1).
func TestDocumentFieldValidates(t *testing.T) {
	et := relatedElementType(relatedSchema)
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"a uid", `{"lead":"01JBXR7M"}`, false},
		{"an empty value", `{"lead":""}`, false},
		{"a uid that does not exist is not this function's business", `{"lead":"01NOPE"}`, false},
		{"a list of uids", `{"related":["01A","01B"]}`, false},
		{"a number", `{"lead":42}`, true},
		{"a path", `{"lead":"/features/film/"}`, true},
		{"a sentence", `{"lead":"see the film piece"}`, true},
		{"a list holding a number", `{"related":["01A",7]}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateContent(et, tc.content)
			if tc.wantErr && err == nil {
				t.Fatal("ValidateContent accepted it, want a refusal")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateContent: %v", err)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalid) {
				t.Errorf("ValidateContent = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestFieldDocumentIsADeclarableType keeps the vocabulary and the validator
// from disagreeing: a type the schema accepts and checkValue ignores would
// accept anything at all in that field.
func TestFieldDocumentIsADeclarableType(t *testing.T) {
	if !ValidFieldType(FieldDocument) {
		t.Fatalf("%q is not a declarable field type", FieldDocument)
	}
	if !slices.Contains(FieldTypes, FieldDocument) {
		t.Errorf("FieldTypes = %v, missing %q", FieldTypes, FieldDocument)
	}
	schema := ElementSchema{Fields: []FieldDef{{Name: "lead", Type: FieldDocument}}}
	if err := schema.Validate(); err != nil {
		t.Errorf("a schema declaring a document field: %v", err)
	}
}

// TestRefusalNamesTheDocument is PLAN.md M10 acceptance 2, 4, and 5's shared
// requirement, at the level of the type that carries it: a refusal a person
// reads names the document and says why.
func TestRefusalNamesTheDocument(t *testing.T) {
	r := Refusal{
		UID:      "01REL",
		Title:    "The Sequel",
		Referrer: "01ROOT",
		Reason:   RefusedCheckedOut,
		Detail:   "somebody has this document checked out",
	}
	got := r.String()
	for _, want := range []string{"01REL", "The Sequel", "01ROOT", "checked out"} {
		if !strings.Contains(got, want) {
			t.Errorf("Refusal.String() = %q, missing %q", got, want)
		}
	}

	// A refusal for a uid naming no document has no title to print, and the
	// rendering must not leave an empty parenthesis behind.
	bare := Refusal{UID: "01GONE", Referrer: "01ROOT", Reason: RefusedMissing, Detail: "no document has this uid"}
	if strings.Contains(bare.String(), "()") {
		t.Errorf("Refusal.String() = %q, want no empty parenthesis", bare.String())
	}
}

// TestRelatedErrorIsAConflict is what makes a refused cascade a 409 through
// the one mapping function at the transport edge (DESIGN.md 14), and what lets
// internal/api fill in the problem document's refusals without knowing
// anything else about the error.
func TestRelatedErrorIsAConflict(t *testing.T) {
	err := error(&RelatedError{
		UID: "01ROOT",
		Refusals: []Refusal{
			{UID: "01A", Reason: RefusedPermission, Detail: "publish is required"},
			{UID: "01B", Reason: RefusedState, Detail: "in draft"},
		},
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("a refused cascade is %v, want ErrConflict", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Error("a refused cascade must not answer to ErrForbidden; it is a statement about the set, not the person")
	}

	refusals, ok := RefusalsOf(err)
	if !ok {
		t.Fatal("RefusalsOf did not recognise a refused cascade")
	}
	if len(refusals) != 2 {
		t.Fatalf("RefusalsOf returned %d refusals, want 2", len(refusals))
	}

	// Every refusal reaches the message, so that a client with no extension
	// member support still sees the names.
	for _, want := range []string{"01ROOT", "01A", "01B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, missing %q", err.Error(), want)
		}
	}

	if _, ok := RefusalsOf(ErrConflict); ok {
		t.Error("RefusalsOf claimed an ordinary conflict was a refused cascade")
	}
}

// TestRefusalReasonsAreClosed keeps the vocabulary honest: a reason a client
// switches on must be one this binary can actually produce, and the list is
// what the CLI and the tests iterate.
func TestRefusalReasonsAreClosed(t *testing.T) {
	seen := map[RefusalReason]bool{}
	for _, r := range RefusalReasons {
		if r == "" {
			t.Error("a refusal reason is the empty string")
		}
		if seen[r] {
			t.Errorf("refusal reason %q is listed twice", r)
		}
		seen[r] = true
	}
	for _, want := range []RefusalReason{
		RefusedMissing, RefusedPermission, RefusedState, RefusedCheckedOut, RefusedNoVersion,
	} {
		if !seen[want] {
			t.Errorf("RefusalReasons is missing %q", want)
		}
	}
}
