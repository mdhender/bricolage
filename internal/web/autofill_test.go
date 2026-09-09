// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"html/template"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Invariant 23: autocomplete is denied by default on every form control.
//
// This test is the enforcement, and it is a source scan rather than a rendering
// one on purpose: what has to hold is that nobody adds a field and forgets, and
// that is a property of the template rather than of one page's data. Rendering
// every page would need a fixture per page and would still miss the branch a
// fixture did not reach.
//
// What it cannot check is the browser. Whether Chrome and 1Password actually
// leave a field alone is a thing a person verifies by looking at it, and the
// four attributes below are the best available answer rather than a guarantee
// -- the managers treat all of this as advisory. That is precisely why the
// policy is deny-by-default: the only field we can be sure a manager will not
// disturb is one we never asked it to fill.

// controlRE finds the opening tag of every form control in a template.
var controlRE = regexp.MustCompile(`(?s)<(input|textarea|select)\b[^>]*>`)

// typeRE reads the type attribute off one of those tags.
var typeRE = regexp.MustCompile(`type="([a-z-]+)"`)

// carryNoValue are the control types a password manager has nothing to put in.
// A hidden field is written by the server, and a checkbox or a button carries a
// state rather than a value somebody would want filled in.
var carryNoValue = map[string]bool{
	"hidden": true, "checkbox": true, "radio": true, "submit": true, "button": true,
}

// TestEveryControlDeclaresAutofill is invariant 23.
//
// A control that declares nothing is the bug: "nothing" is not neutral, it is
// the browser guessing from the field's name and type, and the guess that
// started this was "email" on the invite form -- a box whose one impossible
// value is the address of the person filling it in.
func TestEveryControlDeclaresAutofill(t *testing.T) {
	var checked int
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".gohtml") {
			return err
		}
		body, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		for _, tag := range controlRE.FindAllString(string(body), -1) {
			if m := typeRE.FindStringSubmatch(tag); m != nil && carryNoValue[m[1]] {
				continue
			}
			checked++
			switch {
			case strings.Contains(tag, "{{noAutofill}}"):
				// Denied, which is the default and needs no justification.
			case strings.Contains(tag, "autocomplete="):
				// Opted in. The comment saying why is a review matter; what is
				// asserted here is that the choice was made deliberately.
			default:
				t.Errorf("%s: this control declares no autofill policy:\n\t%s\n"+
					"Add {{noAutofill}} to deny (invariant 23), or an explicit autocomplete "+
					"token plus a comment in the template saying whose data the field holds. "+
					"Declaring nothing means the browser and every password manager guess.",
					path, strings.TrimSpace(tag))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the templates: %v", err)
	}
	if checked == 0 {
		t.Fatal("no form controls were found; this test is asserting nothing")
	}
	t.Logf("%d form controls checked", checked)
}

// TestOnlyCredentialFormsOptIn holds the policy's second half: opting in is
// rare and deliberate.
//
// The two forms named here are the only ones whose fields hold the credentials
// of the person looking at the screen. A third would not be wrong on its face,
// but it should not appear without somebody deciding it -- so this test fails
// and asks, rather than letting the list grow quietly.
func TestOnlyCredentialFormsOptIn(t *testing.T) {
	allowed := map[string]bool{
		"templates/pages/login.gohtml":  true,
		"templates/pages/redeem.gohtml": true,
	}

	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".gohtml") {
			return err
		}
		body, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		for _, tag := range controlRE.FindAllString(string(body), -1) {
			if !strings.Contains(tag, "autocomplete=") || allowed[path] {
				continue
			}
			t.Errorf("%s opts a control in to autofill:\n\t%s\n"+
				"Only the login and redemption forms do that today, because only they hold "+
				"the viewer's own credentials. If this field really is the viewer's own, add "+
				"it here and say why in the template; otherwise deny it with {{noAutofill}}.",
				path, strings.TrimSpace(tag))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the templates: %v", err)
	}

	// The other direction: a form named here that stopped opting in would leave
	// this list asserting nothing.
	for path := range allowed {
		body, err := fs.ReadFile(templateFS, path)
		if err != nil {
			t.Errorf("%s is named as a credential form and is not there: %v", path, err)
			continue
		}
		if !strings.Contains(string(body), "autocomplete=") {
			t.Errorf("%s is named as a credential form but opts nothing in; "+
				"either it should, or it should come off this list", path)
		}
	}
}

// TestNoAutofillEmitsEveryOptOut pins what the helper produces.
//
// The three data- attributes are 1Password's, LastPass's and Dashlane's, and
// they are the only vendor-specific markup in this UI. They look like cruft to
// anybody who has not been shouted at by a fill prompt, and the cost of quietly
// dropping one is that the prompt comes back on one manager and not the others.
func TestNoAutofillEmitsEveryOptOut(t *testing.T) {
	fn, ok := funcs["noAutofill"].(func() template.HTMLAttr)
	if !ok {
		t.Fatal("noAutofill is not registered as a template func returning template.HTMLAttr")
	}
	got := string(fn())
	for _, want := range []string{
		`autocomplete="off"`,
		`data-1p-ignore`,
		`data-lpignore="true"`,
		`data-form-type="other"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("noAutofill does not emit %s; it emits %q", want, got)
		}
	}
}
