package server

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TB-85: registration accepts the per-runtime host budgets and bounds them by
// the machine the way it bounds the single figures — 0 (a client predating
// them) is accepted, a figure above the machine or below zero is refused. It
// reuses registration_admission_test.go's fakes.
func TestTB85_RegistrationBoundsTheHostBudgets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cores, mem  int32
		wantRefusal bool
	}{
		{"within the machine", 8, 32768, false},
		{"not reported", 0, 0, false},
		{"more cores than the machine", 9, 16384, true},
		{"more memory than the machine", 4, 32769, true},
		{"negative memory", 4, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newAdmissionRecordingVolunteerRepo()
			svc := newAdmissionTestService(t, repo)
			pub := newPowKeypair(t)
			req := admissionRegisterReq(pub)
			req.Hardware.HostMaxCpuCores = tc.cores
			req.Hardware.HostMaxMemoryMb = tc.mem
			_, err := svc.RegisterVolunteer(admissionCtx(pub, ""), req)
			if tc.wantRefusal {
				if status.Code(err) != codes.InvalidArgument {
					t.Errorf("host budgets %d cores / %d MB: err = %v, want InvalidArgument", tc.cores, tc.mem, err)
				}
				return
			}
			if err != nil {
				t.Errorf("host budgets %d cores / %d MB refused: %v", tc.cores, tc.mem, err)
			}
		})
	}
}
