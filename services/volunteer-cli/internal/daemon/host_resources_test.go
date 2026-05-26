package daemon

import "testing"

func TestParseMemAvailableMB(t *testing.T) {
	tests := []struct {
		name    string
		meminfo string
		wantMB  int
		wantOK  bool
	}{
		{
			name: "typical meminfo",
			meminfo: "MemTotal:       32827484 kB\n" +
				"MemFree:         1234567 kB\n" +
				"MemAvailable:   30482140 kB\n" +
				"Buffers:          123456 kB\n",
			wantMB: 30482140 / 1024,
			wantOK: true,
		},
		{
			name:    "memavailable first line",
			meminfo: "MemAvailable: 2048 kB\n",
			wantMB:  2, // 2048 kB = 2 MB
			wantOK:  true,
		},
		{
			name:    "field absent",
			meminfo: "MemTotal: 100 kB\nMemFree: 50 kB\n",
			wantMB:  0,
			wantOK:  false,
		},
		{
			name:    "empty",
			meminfo: "",
			wantMB:  0,
			wantOK:  false,
		},
		{
			name:    "no trailing newline",
			meminfo: "MemAvailable:   1048576 kB",
			wantMB:  1024,
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMB, gotOK := parseMemAvailableMB(tt.meminfo)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotOK && gotMB != tt.wantMB {
				t.Errorf("mb = %d, want %d", gotMB, tt.wantMB)
			}
		})
	}
}
