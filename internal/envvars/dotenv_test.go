package envvars

import (
	"reflect"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	text := "# comment\r\n\nexport API_URL=https://api.example.org\nPLAIN = value with spaces # trailing comment\nHASH=abc#def\nSINGLE='literal $HOME \\n'\nDOUBLE=\"line1\\nline2 \\\"quoted\\\"\"\nEMPTY=\nCOLOR=#ff0000\nexport B=#x\nCOMMENTED= # nothing here\nQD=\"a b\" # note\nQS='x y'   # note\nQE=\"a \\\" # b\"\n"
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
		{Name: "COLOR", Value: "#ff0000"},
		{Name: "B", Value: "#x"},
		{Name: "COMMENTED", Value: ""},
		{Name: "QD", Value: "a b"},
		{Name: "QS", Value: "x y"},
		{Name: "QE", Value: `a " # b`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	for _, bad := range []string{"NOEQUALS", "1BAD=x", `Q="open`, "S='open", `E="bad \q"`, "# only comments\n", `A="x" junk`, `A='x' junk`} {
		if _, err := ParseDotEnv(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseDotEnvStripsBOM(t *testing.T) {
	got, err := ParseDotEnv("\ufeffA=1\n")
	if err != nil || len(got) != 1 || got[0].Name != "A" || got[0].Value != "1" {
		t.Fatalf("got %#v, %v", got, err)
	}
}
