package config

import "testing"

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    Size
		wantErr bool
	}{
		{"", 0, false},
		{"1024", 1024, false},
		{"3M", 3 << 20, false},
		{"40G", 40 << 30, false},
		{"1.5G", Size(1.5 * (1 << 30)), false},
		{"512kb", 512 << 10, false},
		{"-1G", 0, true},
		{"nonsense", 0, true},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseSize(%q): expected an error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFormatSelectorRespectsQuality(t *testing.T) {
	cases := []struct {
		name         string
		quality      string
		format       string
		preferDirect bool
		want         string
		wantErr      bool
	}{
		{
			name: "raw format overrides quality", quality: "1080", format: "bv+ba",
			want: "bv+ba",
		},
		{
			name: "720 without protocol preference", quality: "720",
			want: "bv*[height<=720]+ba/b[height<=720]/b",
		},
		{
			name: "360 preferring direct streams", quality: "360", preferDirect: true,
			want: "bv*[height<=360][protocol=https]+ba[protocol=https]/bv*[height<=360]+ba/b[height<=360]/b",
		},
		{name: "p suffix is allowed", quality: "1080p", want: "bv*[height<=1080]+ba/b[height<=1080]/b"},
		{name: "audio only", quality: "audio", want: "ba/b"},
		{name: "garbage quality", quality: "ultra", wantErr: true},
		{name: "zero is rejected", quality: "0", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Download{Quality: c.quality, Format: c.format, PreferDirect: c.preferDirect}
			got, err := d.FormatSelector()
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}
