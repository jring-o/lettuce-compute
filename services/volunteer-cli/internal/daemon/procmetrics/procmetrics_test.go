package procmetrics

import (
	"testing"
)

func TestProcessMetrics_FieldsNilByDefault(t *testing.T) {
	m := &ProcessMetrics{}
	if m.MemoryRSSMB != nil {
		t.Error("MemoryRSSMB should be nil by default")
	}
	if m.VirtualMemoryMB != nil {
		t.Error("VirtualMemoryMB should be nil by default")
	}
	if m.CPUUsagePct != nil {
		t.Error("CPUUsagePct should be nil by default")
	}
	if m.DiskReadMB != nil {
		t.Error("DiskReadMB should be nil by default")
	}
	if m.DiskWrittenMB != nil {
		t.Error("DiskWrittenMB should be nil by default")
	}
}

func TestProcessMetrics_FieldsPopulated(t *testing.T) {
	rss := 128.5
	virt := 512.0
	cpu := 45.2
	read := 10.0
	write := 5.5

	m := &ProcessMetrics{
		MemoryRSSMB:    &rss,
		VirtualMemoryMB: &virt,
		CPUUsagePct:    &cpu,
		DiskReadMB:     &read,
		DiskWrittenMB:  &write,
	}

	if *m.MemoryRSSMB != 128.5 {
		t.Errorf("MemoryRSSMB = %f, want 128.5", *m.MemoryRSSMB)
	}
	if *m.VirtualMemoryMB != 512.0 {
		t.Errorf("VirtualMemoryMB = %f, want 512.0", *m.VirtualMemoryMB)
	}
	if *m.CPUUsagePct != 45.2 {
		t.Errorf("CPUUsagePct = %f, want 45.2", *m.CPUUsagePct)
	}
	if *m.DiskReadMB != 10.0 {
		t.Errorf("DiskReadMB = %f, want 10.0", *m.DiskReadMB)
	}
	if *m.DiskWrittenMB != 5.5 {
		t.Errorf("DiskWrittenMB = %f, want 5.5", *m.DiskWrittenMB)
	}
}

func TestNewReader_ReturnsNonNil(t *testing.T) {
	r := NewReader()
	if r == nil {
		t.Fatal("NewReader() returned nil")
	}
}

func TestReader_InvalidPID(t *testing.T) {
	r := NewReader()
	_, err := r.Read(0)
	if err == nil {
		t.Error("expected error for PID 0")
	}
	_, err = r.Read(-1)
	if err == nil {
		t.Error("expected error for negative PID")
	}
}
