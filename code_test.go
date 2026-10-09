package main

import (
	"strings"
	"testing"
)

func TestGenerateAndParseCode(t *testing.T) {
	for range 200 {
		code, err := generateCode()
		if err != nil {
			t.Fatal(err)
		}
		norm, plate, err := parseCode(code)
		if err != nil {
			t.Fatalf("parseCode(%q): %v", code, err)
		}
		if norm != code || !strings.HasPrefix(code, plate+"-") || len(strings.Split(code, "-")) != 3 {
			t.Fatalf("bad code %q (plate %q)", code, plate)
		}
	}
}

func TestParseCode(t *testing.T) {
	good := map[string][2]string{
		"8-maple-otter":     {"8-maple-otter", "8"},
		"  8-Maple-OTTER\n": {"8-maple-otter", "8"},
		"999-a-b":           {"999-a-b", "999"},
		"007-maple-otter":   {"007-maple-otter", "7"},
	}
	for in, want := range good {
		norm, plate, err := parseCode(in)
		if err != nil || norm != want[0] || plate != want[1] {
			t.Errorf("parseCode(%q) = %q, %q, %v; want %q, %q", in, norm, plate, err, want[0], want[1])
		}
	}
	for _, in := range []string{"", "8", "8-", "maple-otter", "0-maple-otter", "1000-maple-otter", "-1-maple", "x-maple-otter"} {
		if _, _, err := parseCode(in); err == nil {
			t.Errorf("parseCode(%q) should fail", in)
		}
	}
}

func TestParseTarget(t *testing.T) {
	good := map[string]struct {
		addr string
		port int
	}{
		"3000":              {"localhost:3000", 3000},
		":8080":             {"localhost:8080", 8080},
		"192.168.1.20:5432": {"192.168.1.20:5432", 5432},
		"db.internal:5432":  {"db.internal:5432", 5432},
		"[::1]:22":          {"[::1]:22", 22},
	}
	for in, want := range good {
		addr, port, err := parseTarget(in)
		if err != nil || addr != want.addr || port != want.port {
			t.Errorf("parseTarget(%q) = %q, %d, %v", in, addr, port, err)
		}
	}
	for _, in := range []string{"", "abc", "0", "70000", "host:", "host:http"} {
		if _, _, err := parseTarget(in); err == nil {
			t.Errorf("parseTarget(%q) should fail", in)
		}
	}
}
