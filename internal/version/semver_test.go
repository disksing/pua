package version

import "testing"

func TestSemVerPrecedence(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
	}
	for index := 1; index < len(ordered); index++ {
		if Compare(MustParse(ordered[index-1]), MustParse(ordered[index])) >= 0 {
			t.Fatalf("%s is not lower than %s", ordered[index-1], ordered[index])
		}
	}
	if Compare(MustParse("1.0.0+one"), MustParse("1.0.0+two")) != 0 {
		t.Fatal("build metadata affected precedence")
	}
}

func TestSemVerRejectsInvalidForms(t *testing.T) {
	for _, input := range []string{"", "v1.2.3", "1.2", "01.2.3", "1.2.3-01", "1.2.3+", "1.2.3+bad!"} {
		if _, err := Parse(input); err == nil {
			t.Fatalf("Parse(%q) succeeded", input)
		}
	}
}

func TestSemVerAllowsHyphensInPrereleaseIdentifiers(t *testing.T) {
	parsed, err := Parse("1.2.3-alpha-beta.1")
	if err != nil || len(parsed.Prerelease) != 2 || parsed.Prerelease[0].Value != "alpha-beta" {
		t.Fatalf("Parse() = %+v, %v", parsed, err)
	}
}

func TestIsDevelopment(t *testing.T) {
	if !IsDevelopment(MustParse("0.2.0-dev.4+gabc")) || !IsDevelopment(MustParse("0.2.0+gabc.dirty")) {
		t.Fatal("development version was not detected")
	}
	if IsDevelopment(MustParse("0.2.0-rc.1")) || IsDevelopment(MustParse("0.2.0")) {
		t.Fatal("release version was marked development")
	}
}
