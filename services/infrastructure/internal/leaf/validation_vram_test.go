package leaf

import (
	"strings"
	"testing"
)

// TestValidateResourceRequirements_VRAMWholeGiB is the TB-20 authoring guard.
// Graphics memory is sold and reported in whole GiB, so a requirement like 4000
// is not a capacity any card has — and it reads to a volunteer as "a 4 GB card
// will do" when the gate is against their allowed fraction of one. The live
// leaves that prompted this declared exactly 4000, because nothing but a
// non-negative check stood in the way.
func TestValidateResourceRequirements_VRAMWholeGiB(t *testing.T) {
	base := func() *ResourceRequirements {
		return &ResourceRequirements{MinCPUCores: 1, MinDiskMB: 1024, GPURequired: true}
	}

	// Accepted: any whole number of GiB, including the sizes a fixed picklist of
	// "2/4/6/8/12/16/24" would have refused. Real cards ship at 3, 10, 11, 20, 40
	// and 80 GB, and a hardware list would go stale every GPU generation.
	for _, mb := range []int{0, 1024, 2048, 3072, 4096, 6144, 8192, 10240, 11264, 12288, 20480, 40960, 81920} {
		r := base()
		r.MinGPUVRAMMB = mb
		if err := ValidateResourceRequirements(r); err != nil {
			t.Errorf("min_gpu_vram_mb=%d rejected: %v", mb, err)
		}
	}

	// Refused: anything that is not a whole GiB.
	for _, mb := range []int{1, 4000, 4095, 4097, 6000, 8000} {
		r := base()
		r.MinGPUVRAMMB = mb
		err := ValidateResourceRequirements(r)
		if err == nil {
			t.Errorf("min_gpu_vram_mb=%d accepted, want rejected", mb)
			continue
		}
		if !strings.Contains(err.Error(), "min_gpu_vram_mb") {
			t.Errorf("min_gpu_vram_mb=%d error does not name the field: %v", mb, err)
		}
	}

	// The suggestion rounds UP: the value is a floor a machine must clear, so
	// proposing 3072 for 4000 would silently admit cards that cannot hold the work.
	r := base()
	r.MinGPUVRAMMB = 4000
	err := ValidateResourceRequirements(r)
	if err == nil {
		t.Fatal("4000 accepted")
	}
	if !strings.Contains(err.Error(), "4096") {
		t.Errorf("error should suggest 4096 (rounded up), got: %v", err)
	}
	if strings.Contains(err.Error(), "3072") {
		t.Errorf("error suggests rounding DOWN, which would admit machines the leaf cannot run on: %v", err)
	}

	// A negative value keeps its own, more specific error rather than being
	// swallowed by the modulo check.
	r = base()
	r.MinGPUVRAMMB = -1
	if err := ValidateResourceRequirements(r); err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Errorf("negative min_gpu_vram_mb: got %v, want the non-negative error", err)
	}
}

// TestWholeGiBToCover pins the rounding direction on its own, because the
// suggestion is the number a leaf author will paste.
func TestWholeGiBToCover(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{1, 1024}, {1024, 1024}, {1025, 2048}, {4000, 4096}, {4096, 4096}, {6000, 6144}, {0, 0},
	} {
		if got := wholeGiBToCover(tc.in); got != tc.want {
			t.Errorf("wholeGiBToCover(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
