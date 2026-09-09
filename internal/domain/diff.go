// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"strings"
)

// Word-level diff between two versions (PLAN.md M3).
//
// It is here, in domain, because it is a pure function over two values and
// because the API, the CLI, and eventually the HTMX UI must all show the same
// answer. The rendering is git's "--word-diff=plain" notation -- deletions in
// [-brackets-], insertions in {+braces+} -- because it is a format editors
// already read and because it is stable enough to keep in a golden file
// (acceptance 7).
//
// Word level rather than line level is the point. Editorial changes are
// changes to sentences, and a line diff of a paragraph that somebody rewrapped
// says the whole paragraph changed.

// DiffKind says what happened to a run of words.
type DiffKind int

const (
	// DiffEqual is text present in both versions.
	DiffEqual DiffKind = iota

	// DiffDelete is text in the older version only.
	DiffDelete

	// DiffInsert is text in the newer version only.
	DiffInsert
)

// String returns the marker the rendering uses, which is also what the JSON
// representation carries: "=", "-", "+".
func (k DiffKind) String() string {
	switch k {
	case DiffEqual:
		return "="
	case DiffDelete:
		return "-"
	case DiffInsert:
		return "+"
	default:
		return fmt.Sprintf("DiffKind(%d)", int(k))
	}
}

// DiffOp is one run of words that were kept, removed, or added.
type DiffOp struct {
	Kind DiffKind

	// Text is the run as it should be shown: words and the whitespace between
	// them. Concatenating the Equal and Delete runs reproduces the older
	// text's words; concatenating Equal and Insert reproduces the newer
	// text exactly. Whitespace is a delimiter rather than content, so the
	// separators shown are always the newer text's.
	Text string
}

// FieldDiff is the diff of one field of a version.
type FieldDiff struct {
	// Field is the name a person reads: "title", "slug", "cover_date",
	// "content".
	Field string

	Ops []DiffOp

	// Changed is false when the field is identical in both versions. An
	// unchanged field is still reported, so that a caller rendering a form
	// knows the field was compared and found equal rather than skipped.
	Changed bool
}

// diffFields are the fields compared, in the order they are shown. The
// check-in note is deliberately absent: it describes the act of checking in,
// not the content, and diffing it would report a change every time somebody
// typed a different message about the same words.
var diffFields = []string{"title", "slug", "cover_date", "content"}

func (v Version) diffField(name string) string {
	switch name {
	case "title":
		return v.Title
	case "slug":
		return v.Slug
	case "cover_date":
		return v.CoverDate
	case "content":
		return v.Content
	default:
		return ""
	}
}

// DiffVersions compares two versions field by field.
//
// It is pure and total: any two versions produce an answer, including two
// copies of the same one, which produces four unchanged fields.
func DiffVersions(from, to Version) []FieldDiff {
	out := make([]FieldDiff, 0, len(diffFields))
	for _, name := range diffFields {
		a, b := from.diffField(name), to.diffField(name)
		fd := FieldDiff{Field: name, Changed: a != b}
		if fd.Changed {
			fd.Ops = DiffWords(a, b)
		} else {
			fd.Ops = []DiffOp{{Kind: DiffEqual, Text: a}}
		}
		out = append(out, fd)
	}
	return out
}

// RenderDiff renders a version diff as text (acceptance 7).
//
// The output is stable: the same two versions always render identically,
// which is what lets a golden file be the test. Unchanged fields are omitted,
// because a diff that reprints everything is a diff nobody reads.
func RenderDiff(fromNumber, toNumber int, fields []FieldDiff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- version %d\n", fromNumber)
	fmt.Fprintf(&b, "+++ version %d\n", toNumber)

	changed := 0
	for _, f := range fields {
		if !f.Changed {
			continue
		}
		changed++
		fmt.Fprintf(&b, "@@ %s @@\n", f.Field)
		for i, op := range f.Ops {
			// A deletion immediately followed by its replacement drops its
			// trailing separator: the insertion carries the one the reader
			// should see, and keeping both renders "[-Quick-] {+Slow+}" with
			// a space that is in neither version.
			replaced := op.Kind == DiffDelete && i+1 < len(f.Ops) && f.Ops[i+1].Kind == DiffInsert
			b.WriteString(markup(op, replaced))
		}
		b.WriteString("\n")
	}
	if changed == 0 {
		fmt.Fprintf(&b, "no differences between version %d and version %d\n", fromNumber, toNumber)
	}
	return b.String()
}

// markup wraps one run in its notation, keeping the trailing whitespace
// outside the markers so that the result reads as prose rather than as a
// column of brackets.
func markup(op DiffOp, dropTail bool) string {
	if op.Kind == DiffEqual {
		return op.Text
	}
	body := strings.TrimRight(op.Text, " \t\r\n")
	tail := op.Text[len(body):]
	if dropTail {
		tail = ""
	}
	switch op.Kind {
	case DiffDelete:
		return "[-" + body + "-]" + tail
	case DiffInsert:
		return "{+" + body + "+}" + tail
	default:
		return op.Text
	}
}

// token is one word and the whitespace that follows it.
//
// Comparing words and carrying separators alongside is what makes a rewrap
// invisible: re-flowing a paragraph changes which spaces are newlines and
// changes no words, so the diff is empty. Whitespace is a delimiter here, not
// content.
type token struct {
	word string
	sep  string
}

// tokenize splits s into a leading run of whitespace and a list of words with
// their following separators. Reassembling prefix and every word+sep
// reproduces s exactly.
func tokenize(s string) (prefix string, toks []token) {
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	prefix = s[:i]
	for i < len(s) {
		start := i
		for i < len(s) && !isSpace(s[i]) {
			i++
		}
		word := s[start:i]
		start = i
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		toks = append(toks, token{word: word, sep: s[start:i]})
	}
	return prefix, toks
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// maxCells bounds the longest-common-subsequence matrix.
//
// The algorithm is O(n*m) in time and space after the common prefix and
// suffix have been trimmed, which is cheap for an edit and expensive for two
// unrelated documents. Beyond the bound the answer degrades to "this was
// replaced", which is both true and what a person would say about two texts
// with nothing in common. A million cells is a few megabytes and covers a
// long feature rewritten twice over.
const maxCells = 1 << 20

// DiffWords returns the word-level difference between two texts.
//
// Equal runs are shared, deletions come from the older text and insertions
// from the newer, and a deletion is always emitted before the insertion that
// replaced it, so the output order is deterministic.
func DiffWords(from, to string) []DiffOp {
	// Leading whitespace is not a word, so it never participates in the
	// match. The newer text's is what a reader sees.
	_, a := tokenize(from)
	toPrefix, b := tokenize(to)

	// Trim the common head and tail before doing anything quadratic. Editorial
	// changes are local, so this is usually most of the work.
	head := 0
	for head < len(a) && head < len(b) && a[head].word == b[head].word {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head &&
		a[len(a)-1-tail].word == b[len(b)-1-tail].word {
		tail++
	}

	var ops []DiffOp
	emit := func(kind DiffKind, toks []token) {
		if len(toks) == 0 {
			return
		}
		var sb strings.Builder
		for _, t := range toks {
			sb.WriteString(t.word)
			sb.WriteString(t.sep)
		}
		ops = appendOp(ops, DiffOp{Kind: kind, Text: sb.String()})
	}

	if toPrefix != "" {
		ops = appendOp(ops, DiffOp{Kind: DiffEqual, Text: toPrefix})
	}
	// The head is shared, so it is rendered from the newer side.
	emit(DiffEqual, b[:head])

	midA := a[head : len(a)-tail]
	midB := b[head : len(b)-tail]

	if len(midA) > 0 && len(midB) > 0 && len(midA)*len(midB) > maxCells {
		emit(DiffDelete, midA)
		emit(DiffInsert, midB)
	} else {
		for _, run := range lcsDiff(midA, midB) {
			emit(run.kind, run.toks)
		}
	}

	emit(DiffEqual, b[len(b)-tail:])
	return ops
}

// appendOp adds an op, merging it into the previous one when the kinds match.
// Adjacent runs of the same kind are one run: two of them would render as two
// pairs of brackets around one phrase.
func appendOp(ops []DiffOp, op DiffOp) []DiffOp {
	if op.Text == "" {
		return ops
	}
	if n := len(ops); n > 0 && ops[n-1].Kind == op.Kind {
		ops[n-1].Text += op.Text
		return ops
	}
	return append(ops, op)
}

// run is a stretch of tokens with one fate.
type run struct {
	kind DiffKind
	toks []token
}

// lcsDiff is the longest-common-subsequence diff of two token slices.
//
// It is the textbook dynamic program. It is here rather than in a dependency
// because the whole of it is forty lines, because the output has to be stable
// across releases for the golden file to mean anything, and because a
// third-party diff is a third-party opinion about what a word is.
func lcsDiff(a, b []token) []run {
	switch {
	case len(a) == 0 && len(b) == 0:
		return nil
	case len(a) == 0:
		return []run{{DiffInsert, b}}
	case len(b) == 0:
		return []run{{DiffDelete, a}}
	}

	// lengths[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:]. Filling from the end lets the walk below go forwards, which
	// is what keeps deletions ahead of insertions.
	width := len(b) + 1
	lengths := make([]uint32, (len(a)+1)*width)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i].word == b[j].word {
				lengths[i*width+j] = lengths[(i+1)*width+j+1] + 1
				continue
			}
			down, right := lengths[(i+1)*width+j], lengths[i*width+j+1]
			if down >= right {
				lengths[i*width+j] = down
			} else {
				lengths[i*width+j] = right
			}
		}
	}

	var runs []run
	add := func(kind DiffKind, t token) {
		if n := len(runs); n > 0 && runs[n-1].kind == kind {
			runs[n-1].toks = append(runs[n-1].toks, t)
			return
		}
		runs = append(runs, run{kind: kind, toks: []token{t}})
	}

	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].word == b[j].word:
			// The equal token is taken from the newer side, so the separators
			// a reader sees are the ones the newer text uses.
			add(DiffEqual, b[j])
			i, j = i+1, j+1
		case lengths[(i+1)*width+j] >= lengths[i*width+j+1]:
			add(DiffDelete, a[i])
			i++
		default:
			add(DiffInsert, b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		add(DiffDelete, a[i])
	}
	for ; j < len(b); j++ {
		add(DiffInsert, b[j])
	}
	return runs
}
