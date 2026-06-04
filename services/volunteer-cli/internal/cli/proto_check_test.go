package cli

import (
	"testing"

	// Compile check: verify that the infrastructure proto package is importable
	// via the Go workspace. Actual gRPC calls are implemented in S25.
	pb "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

func TestProtoImportCompiles(t *testing.T) {
	// Verify we can reference proto types from the infrastructure module.
	_ = &pb.RegisterVolunteerRequest{}
	_ = &pb.StartWorkRequest{}
	_ = &pb.RequestWorkUnitRequest{}
	_ = &pb.SubmitResultRequest{}
}
