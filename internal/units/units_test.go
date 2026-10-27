package units

import "testing"

func TestParseSize(t *testing.T) {
	cases := map[string]uint64{
		"4G": 4 << 30, "512M": 512 << 20, "1024K": 1 << 20, "2T": 2 << 40,
		"1024": 1024 << 20, "1.5G": 3 << 29,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1G", "4X", "G"} {
		if got, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = %d, want error", in, got)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[uint64]string{4 << 30: "4.0 GiB", 512 << 20: "512 MiB"}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}
