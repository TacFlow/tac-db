package tacdb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

// TestEveryCodeHasConstantAndSentinel fails when a code is added to
// internal/errs without its public constant and sentinel.
func TestEveryCodeHasConstantAndSentinel(t *testing.T) {
	table := []struct {
		code     Code
		sentinel *Error
	}{
		{CodeClosed, ErrClosed},
		{CodeBusy, ErrBusy},
		{CodeTxClosed, ErrTxClosed},
		{CodeTxInUse, ErrTxInUse},
		{CodeReadOnly, ErrReadOnly},
		{CodeDBBusy, ErrDBBusy},
		{CodeBusyTimeout, ErrBusyTimeout},
		{CodeWriterQueueFull, ErrWriterQueueFull},
		{CodeTooManyTransactions, ErrTooManyTransactions},
		{CodeSnapshotExpired, ErrSnapshotExpired},
		{CodeWriteTxTimeout, ErrWriteTxTimeout},
		{CodeShuttingDown, ErrShuttingDown},
		{CodeInvalidArgument, ErrInvalidArgument},
		{CodeTypeMismatch, ErrTypeMismatch},
		{CodeLimitExceeded, ErrLimitExceeded},
		{CodeNotFound, ErrNotFound},
		{CodeDuplicateKey, ErrDuplicateKey},
		{CodeQueryLex, ErrQueryLex},
		{CodeQueryParse, ErrQueryParse},
		{CodeQuerySemantic, ErrQuerySemantic},
		{CodeQueryUnsupported, ErrQueryUnsupported},
		{CodeQueryWriteRequiresExec, ErrQueryWriteRequiresExec},
		{CodeParamMissing, ErrParamMissing},
		{CodeParamType, ErrParamType},
		{CodeLocked, ErrLocked},
		{CodeIO, ErrIO},
		{CodeNoSpace, ErrNoSpace},
		{CodeCorrupt, ErrCorrupt},
		{CodeUnsupportedFormat, ErrUnsupportedFormat},
		{CodeWALLimit, ErrWALLimit},
		{CodeCommitUnknown, ErrCommitUnknown},
		{CodeCommitPending, ErrCommitPending},
		{CodeCommitQueueFull, ErrCommitQueueFull},
		{CodeRecoveryRequired, ErrRecoveryRequired},
		{CodeBackupLag, ErrBackupLag},
		{CodeBackupInvalid, ErrBackupInvalid},
		{CodeDestinationExists, ErrDestinationExists},
		{CodeIndexNotFound, ErrIndexNotFound},
		{CodeIndexExistsDifferent, ErrIndexExistsDifferent},
		{CodeInvalidVector, ErrInvalidVector},
		{CodeVectorDisabled, ErrVectorDisabled},
		{CodeVectorKeyMismatch, ErrVectorKeyMismatch},
		{CodeIndexInvalid, ErrIndexInvalid},
		{CodeIndexInconsistent, ErrIndexInconsistent},
		{CodeIndexBuilding, ErrIndexBuilding},
		{CodeIndexBuildFailed, ErrIndexBuildFailed},
		{CodeUniqueViolation, ErrUniqueViolation},
		{CodeReservedStream, ErrReservedStream},
		{CodeCursorExpired, ErrCursorExpired},
		{CodeOffsetRegression, ErrOffsetRegression},
		{CodeInvalidSequence, ErrInvalidSequence},
		{CodeCanceled, ErrCanceled},
		{CodeDeadline, ErrDeadline},
		{CodeResourceLimit, ErrResourceLimit},
		{CodeQueryIncomplete, ErrQueryIncomplete},
		{CodeNotLeader, ErrNotLeader},
		{CodeNoQuorum, ErrNoQuorum},
		{CodeLeaderChanged, ErrLeaderChanged},
		{CodeReplicaLag, ErrReplicaLag},
		{CodeSessionExpired, ErrSessionExpired},
		{CodeCursorLost, ErrCursorLost},
		{CodeClusterModeRequired, ErrClusterModeRequired},
		{CodeVersionIncompatible, ErrVersionIncompatible},
		{CodeInvalidDurabilityMode, ErrInvalidDurabilityMode},
		{CodeRequestConflict, ErrRequestConflict},
		{CodeRequestOutOfOrder, ErrRequestOutOfOrder},
		{CodeResultExpired, ErrResultExpired},
		{CodeRequestAlreadyApplied, ErrRequestAlreadyApplied},
		{CodeUnauthenticated, ErrUnauthenticated},
		{CodePermissionDenied, ErrPermissionDenied},
		{CodePolicyViolation, ErrPolicyViolation},
		{CodeClassifiedValue, ErrClassifiedValue},
		{CodeQuotaExceeded, ErrQuotaExceeded},
		{CodeSignatureRequired, ErrSignatureRequired},
		{CodeSignatureInvalid, ErrSignatureInvalid},
		{CodeUnsafePath, ErrUnsafePath},
		{CodeInsecurePermissions, ErrInsecurePermissions},
	}
	if len(table) != len(errs.AllCodes) {
		t.Fatalf("public table has %d codes, internal/errs has %d", len(table), len(errs.AllCodes))
	}
	for i, row := range table {
		if row.code != errs.AllCodes[i] {
			t.Errorf("row %d: code %s, want %s (same order as SPEC §6.1)", i, row.code, errs.AllCodes[i])
		}
		if row.sentinel == nil || row.sentinel.Code != row.code {
			t.Errorf("sentinel for %s is wrong: %#v", row.code, row.sentinel)
		}
	}
}

func TestSentinelMatchesWrappedError(t *testing.T) {
	err := fmt.Errorf("context: %w", errs.New(errs.CodeLocked, "vfs.lock", "held by another process"))
	if !errors.Is(err, ErrLocked) {
		t.Fatal("errors.Is(err, ErrLocked) = false")
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeLocked {
		t.Fatalf("errors.As did not expose the LOCKED error: %v", err)
	}
}
