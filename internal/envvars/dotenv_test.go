package envvars

import (
	"reflect"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	text := "# comment\r\n\nexport API_URL=https://api.example.org\nPLAIN = value with spaces # trailing comment\nHASH=abc#def\nSINGLE='literal $HOME \\n'\nDOUBLE=\"line1\\nline2 \\\"quoted\\\"\"\nEMPTY=\n"
	got, err := ParseDotEnv(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []Update{
		{Name: "API_URL", Value: "https://api.example.org"},
		{Name: "PLAIN", Value: "value with spaces"},
		{Name: "HASH", Value: "abc#def"},
		{Name: "SINGLE", Value: `literal $HOME \n`},
		{Name: "DOUBLE", Value: "line1\nline2 \"quoted\""},
		{Name: "EMPTY", Value: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	for _, bad := range []string{"NOEQUALS", "1BAD=x", `Q="open`, "S='open", `E="bad \q"`, "# only comments\n"} {
		if _, err := ParseDotEnv(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
