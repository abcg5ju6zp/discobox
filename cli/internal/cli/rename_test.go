package cli

import (
	"strings"
	"testing"
)

// A rename sends the name as the meta change, to the discobox the listing
// shows by its agent's title, and prints what it is then listed as.
func TestRenameNamesADiscobox(t *testing.T) {
	var calls []string
	server := tagTestServer(t, &calls)

	out, errOut, err := runRM(t, server.URL, "rename", "Fixing the flaky test", "  Daily GH check-in ")
	if err != nil {
		t.Fatalf("execute rename: %v\n%s", err, errOut)
	}
	if got, want := strings.Join(calls, ","), `list,meta `+rmDocsID+` {"name":"Daily GH check-in"}`; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if got, want := out, "Daily GH check-in\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// With no NAME the name is cleared: an empty one is sent, and what is printed
// is what the discobox falls back to being listed as. A full ID needs no
// listing.
func TestRenameWithNoNameClearsIt(t *testing.T) {
	var calls []string
	server := tagTestServer(t, &calls)

	if _, errOut, err := runRM(t, server.URL, "rename", rmDocsID, "Daily GH check-in"); err != nil {
		t.Fatalf("execute rename: %v\n%s", err, errOut)
	}
	calls = nil
	out, errOut, err := runRM(t, server.URL, "-o", "json", "rename", rmDocsID)
	if err != nil {
		t.Fatalf("execute rename: %v\n%s", err, errOut)
	}
	if got, want := strings.Join(calls, ","), `meta `+rmDocsID+` {"name":""}`; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	for _, want := range []string{`"id": "` + rmDocsID + `"`, `"name": ""`, `"displayName": "docs"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %s, want it to hold %s", out, want)
		}
	}
}

// A name that cannot be one is refused as typed, before the listing or the
// discobox is asked anything.
func TestRenameRefusesANameThatIsNotOne(t *testing.T) {
	for _, tc := range []struct{ name, bad, want string }{
		{"two lines", "a\nb", "one line"},
		{"too long", strings.Repeat("n", 129), "longer than"},
		{"only spaces", "   ", "leave NAME out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			server := tagTestServer(t, &calls)
			_, _, err := runRM(t, server.URL, "rename", rmDocsID, tc.bad)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rename error = %v, want it to mention %q", err, tc.want)
			}
			if len(calls) != 0 {
				t.Fatalf("calls = %v, want nothing sent", calls)
			}
		})
	}
}
