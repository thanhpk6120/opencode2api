package config

import "testing"

// The shorthand form keeps "#pool" glued to the password: splitProxyShorthand
// splits on ':' only, and stripProxyLineComment leaves a '#' that has no space
// before it. The entry then parses without error but carries a corrupted
// password, so "#pool" only works in the URL form.
func TestShorthandKeepsPoolMarkerInsidePassword(t *testing.T) {
	got, err := ParseProxyLine("103.69.188.53:40719:sp07v6-40719:PVAFH#pool")
	if err != nil {
		t.Fatalf("ParseProxyLine: %v", err)
	}
	if got != "http://sp07v6-40719:PVAFH%23pool@103.69.188.53:40719" {
		t.Fatalf("shorthand result changed: %q", got)
	}
	_, pool := ParseProxyEntry(got)
	if pool {
		t.Fatal("#pool survived the shorthand form: the recorded behavior changed")
	}

	// The URL form is the one that actually marks the pool.
	url := "http://sp07v6-40719:PVAFH@103.69.188.53:40719#pool"
	clean, pool := ParseProxyEntry(url)
	if !pool || clean != "http://sp07v6-40719:PVAFH@103.69.188.53:40719" {
		t.Fatalf("URL form did not mark the pool: clean=%q pool=%v", clean, pool)
	}
}
