package bytesize

import "testing"

func TestParse(t *testing.T) {
	tests := map[string]int64{
		"4096":   4096,
		"1KiB":   1024,
		"1.5MiB": 1572864,
		"2GB":    2_000_000_000,
	}
	for input, want := range tests {
		got, err := Parse(input)
		if err != nil {
			t.Fatalf("Parse(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("Parse(%q)=%d, want %d", input, got, want)
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, input := range []string{"", "nope", "1XB", "-1GiB"} {
		if _, err := Parse(input); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", input)
		}
	}
}
