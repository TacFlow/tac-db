package errs

// Code is a stable error code (SPEC §6.1). Codes never change once
// released; messages may.
type Code string

// Lifecycle.
const (
	CodeClosed              Code = "CLOSED"
	CodeBusy                Code = "BUSY"
	CodeTxClosed            Code = "TX_CLOSED"
	CodeTxInUse             Code = "TX_IN_USE"
	CodeReadOnly            Code = "READ_ONLY"
	CodeDBBusy              Code = "DB_BUSY"
	CodeBusyTimeout         Code = "BUSY_TIMEOUT"
	CodeWriterQueueFull     Code = "WRITER_QUEUE_FULL"
	CodeTooManyTransactions Code = "TOO_MANY_TRANSACTIONS"
	CodeSnapshotExpired     Code = "SNAPSHOT_EXPIRED"
	CodeWriteTxTimeout      Code = "WRITE_TX_TIMEOUT"
	CodeShuttingDown        Code = "SHUTTING_DOWN"
)

// Input.
const (
	CodeInvalidArgument Code = "INVALID_ARGUMENT"
	CodeTypeMismatch    Code = "TYPE_MISMATCH"
	CodeLimitExceeded   Code = "LIMIT_EXCEEDED"
	CodeNotFound        Code = "NOT_FOUND"
	CodeDuplicateKey    Code = "DUPLICATE_KEY"
)

// Query.
const (
	CodeQueryLex               Code = "QUERY_LEX"
	CodeQueryParse             Code = "QUERY_PARSE"
	CodeQuerySemantic          Code = "QUERY_SEMANTIC"
	CodeQueryUnsupported       Code = "QUERY_UNSUPPORTED"
	CodeQueryWriteRequiresExec Code = "QUERY_WRITE_REQUIRES_EXEC"
	CodeParamMissing           Code = "PARAM_MISSING"
	CodeParamType              Code = "PARAM_TYPE"
)

// Persistence.
const (
	CodeLocked            Code = "LOCKED"
	CodeIO                Code = "IO"
	CodeNoSpace           Code = "NO_SPACE"
	CodeCorrupt           Code = "CORRUPT"
	CodeUnsupportedFormat Code = "UNSUPPORTED_FORMAT"
	CodeWALLimit          Code = "WAL_LIMIT"
	CodeCommitUnknown     Code = "COMMIT_UNKNOWN"
	CodeCommitPending     Code = "COMMIT_PENDING"
	CodeCommitQueueFull   Code = "COMMIT_QUEUE_FULL"
	CodeRecoveryRequired  Code = "RECOVERY_REQUIRED"
)

// Backup.
const (
	CodeBackupLag         Code = "BACKUP_LAG"
	CodeBackupInvalid     Code = "BACKUP_INVALID"
	CodeDestinationExists Code = "DESTINATION_EXISTS"
)

// Indexes.
const (
	CodeIndexNotFound        Code = "INDEX_NOT_FOUND"
	CodeIndexExistsDifferent Code = "INDEX_EXISTS_DIFFERENT"
	CodeInvalidVector        Code = "INVALID_VECTOR"
	CodeVectorDisabled       Code = "VECTOR_DISABLED"
	CodeVectorKeyMismatch    Code = "VECTOR_KEY_MISMATCH"
	CodeIndexInvalid         Code = "INDEX_INVALID"
	CodeIndexInconsistent    Code = "INDEX_INCONSISTENT"
	CodeIndexBuilding        Code = "INDEX_BUILDING"
	CodeIndexBuildFailed     Code = "INDEX_BUILD_FAILED"
	CodeUniqueViolation      Code = "UNIQUE_VIOLATION"
)

// Events.
const (
	CodeReservedStream   Code = "RESERVED_STREAM"
	CodeCursorExpired    Code = "CURSOR_EXPIRED"
	CodeOffsetRegression Code = "OFFSET_REGRESSION"
	CodeInvalidSequence  Code = "INVALID_SEQUENCE"
)

// Execution.
const (
	CodeCanceled        Code = "CANCELED"
	CodeDeadline        Code = "DEADLINE"
	CodeResourceLimit   Code = "RESOURCE_LIMIT"
	CodeQueryIncomplete Code = "QUERY_INCOMPLETE"
)

// Cluster and IPC.
const (
	CodeNotLeader             Code = "NOT_LEADER"
	CodeNoQuorum              Code = "NO_QUORUM"
	CodeLeaderChanged         Code = "LEADER_CHANGED"
	CodeReplicaLag            Code = "REPLICA_LAG"
	CodeSessionExpired        Code = "SESSION_EXPIRED"
	CodeCursorLost            Code = "CURSOR_LOST"
	CodeClusterModeRequired   Code = "CLUSTER_MODE_REQUIRED"
	CodeVersionIncompatible   Code = "VERSION_INCOMPATIBLE"
	CodeInvalidDurabilityMode Code = "INVALID_DURABILITY_MODE"
	CodeRequestConflict       Code = "REQUEST_CONFLICT"
	CodeRequestOutOfOrder     Code = "REQUEST_OUT_OF_ORDER"
	CodeResultExpired         Code = "RESULT_EXPIRED"
	CodeRequestAlreadyApplied Code = "REQUEST_ALREADY_APPLIED"
)

// Security.
const (
	CodeUnauthenticated     Code = "UNAUTHENTICATED"
	CodePermissionDenied    Code = "PERMISSION_DENIED"
	CodePolicyViolation     Code = "POLICY_VIOLATION"
	CodeClassifiedValue     Code = "CLASSIFIED_VALUE"
	CodeQuotaExceeded       Code = "QUOTA_EXCEEDED"
	CodeSignatureRequired   Code = "SIGNATURE_REQUIRED"
	CodeSignatureInvalid    Code = "SIGNATURE_INVALID"
	CodeUnsafePath          Code = "UNSAFE_PATH"
	CodeInsecurePermissions Code = "INSECURE_PERMISSIONS"
)

// AllCodes lists every code in the order of SPEC §6.1.
var AllCodes = []Code{
	CodeClosed, CodeBusy, CodeTxClosed, CodeTxInUse, CodeReadOnly, CodeDBBusy,
	CodeBusyTimeout, CodeWriterQueueFull, CodeTooManyTransactions,
	CodeSnapshotExpired, CodeWriteTxTimeout, CodeShuttingDown,

	CodeInvalidArgument, CodeTypeMismatch, CodeLimitExceeded, CodeNotFound,
	CodeDuplicateKey,

	CodeQueryLex, CodeQueryParse, CodeQuerySemantic, CodeQueryUnsupported,
	CodeQueryWriteRequiresExec, CodeParamMissing, CodeParamType,

	CodeLocked, CodeIO, CodeNoSpace, CodeCorrupt, CodeUnsupportedFormat,
	CodeWALLimit, CodeCommitUnknown, CodeCommitPending, CodeCommitQueueFull,
	CodeRecoveryRequired,

	CodeBackupLag, CodeBackupInvalid, CodeDestinationExists,

	CodeIndexNotFound, CodeIndexExistsDifferent, CodeInvalidVector,
	CodeVectorDisabled, CodeVectorKeyMismatch, CodeIndexInvalid,
	CodeIndexInconsistent, CodeIndexBuilding, CodeIndexBuildFailed,
	CodeUniqueViolation,

	CodeReservedStream, CodeCursorExpired, CodeOffsetRegression,
	CodeInvalidSequence,

	CodeCanceled, CodeDeadline, CodeResourceLimit, CodeQueryIncomplete,

	CodeNotLeader, CodeNoQuorum, CodeLeaderChanged, CodeReplicaLag,
	CodeSessionExpired, CodeCursorLost, CodeClusterModeRequired,
	CodeVersionIncompatible, CodeInvalidDurabilityMode, CodeRequestConflict,
	CodeRequestOutOfOrder, CodeResultExpired, CodeRequestAlreadyApplied,

	CodeUnauthenticated, CodePermissionDenied, CodePolicyViolation,
	CodeClassifiedValue, CodeQuotaExceeded, CodeSignatureRequired,
	CodeSignatureInvalid, CodeUnsafePath, CodeInsecurePermissions,
}
