# M1a — Valores, codec e erros: plano de implementação

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Criar o módulo Go do tacdb com a API de erros, o pacote público `value` e o codec G2 de valores, uvarints e chaves compostas, mais a documentação mínima e o CI.

**Architecture:** Primeiro dos cinco planos do M1 (M1a valores e codec → M1b VFS e locks → M1c páginas e superbloco → M1d governador e cache → M1e alocador, B+tree e checker). Códigos e `Error` ficam em `internal/errs` para que os pacotes internos os usem sem importar a raiz; a raiz `tacdb` os reexporta. `value` é público e imutável; `internal/codec` implementa as codificações da SPEC §5.3, §7.1 e §7.4. Fonte normativa: `SPEC.md` 1.3; design: `docs/superpowers/specs/2026-10-24-m1-fundacao-design.md` (§4.1 a §4.3, §7).

**Tech Stack:** Go 1.27 (toolchain 1.27.3), só biblioteca padrão; GitHub Actions; `govulncheck` v1.9.0.

---

## Regras para quem executa

- **Nunca** escreva o nome do motor de referência em código, comentário, documentação, nome de arquivo, pacote ou função (SPEC §2.1). Diga “motor de referência” ou “R0”.
- Código e comentários em inglês; documentos (`*.md`) em português; mensagens de commit em português, no estilo dos commits existentes.
- Todo arquivo `.go` precisa passar em `gofmt -l .` sem saída. Rode isso antes de cada commit.
- Trabalhe no branch `m1-fundacao`. Não faça push.
- O código deste plano já foi compilado e testado com Go 1.27.3; se um passo não der o resultado esperado, pare e investigue em vez de adaptar o código.

## Estrutura de arquivos

| Arquivo | Responsabilidade |
|---|---|
| `go.mod` | Módulo `github.com/TacFlow/tacdb`, `go 1.27`, `toolchain go1.27.3`. |
| `.gitattributes` | LF em todo texto, em qualquer sistema (o checkout do Windows converteria para CRLF). |
| `doc.go` | Documentação do pacote raiz. |
| `errors.go` | API pública de erros: aliases `Code`, `Error`, `Location`, as 77 constantes `CodeX` e as sentinelas `ErrX`. |
| `internal/errs/codes.go` | Os 77 códigos da SPEC §6.1 e `AllCodes`. |
| `internal/errs/errs.go` | `Error` (com `Details`), construtores, `Is`, `Unwrap`, `CodeOf`. |
| `value/value.go` | `Kind`, `Value`, construtores que copiam, acessores, `Identical`, `String`. |
| `value/compare.go` | `Compare`, a ordem total da SPEC §5.3. |
| `value/limits.go` | `Limits`, `DefaultLimits`, `Validate`. |
| `internal/valuetest/gen.go` | Gerador de valores aleatórios para testes. |
| `internal/codec/uvarint.go` | Uvarint mínimo. |
| `internal/codec/value.go` | Codec G2 de valores com `Budget`. |
| `internal/codec/key.go` | Chaves compostas de B+tree. |
| `internal/codec/testdata/golden/values.golden` | Bytes esperados de valores representativos. |
| `NOTICE`, `FORMAT.md`, `COMPATIBILITY.md`, `AUDIT-REGRESSIONS.md`, `README.md` | Documentação mínima do M0. |
| `.github/workflows/ci.yml`, `scripts/check-nocgo.sh` | Gates de CI da SPEC §25.4 aplicáveis ao M1a. |

### Task 1: Esqueleto do módulo

**Files:**
- Create: `go.mod`
- Create: `.gitattributes`
- Create: `.gitignore`
- Create: `doc.go`
- Modify: `README.md` (substitui o conteúdo inteiro)

- [ ] **Step 1: Criar o go.mod**

Criar `go.mod` com este conteúdo:

````text
module github.com/TacFlow/tacdb

go 1.27

toolchain go1.27.3
````

- [ ] **Step 2: Criar o .gitattributes**

Criar `.gitattributes` com este conteúdo:

````text
# Todo texto do repositório usa LF em qualquer sistema; o checkout do
# Windows converteria para CRLF e quebraria o gofmt e os goldens.
* text=auto eol=lf
*.bin binary
````

Sem ele, o checkout do CI no Windows converte os arquivos para CRLF, e `gofmt -l` e o teste de golden falham.

- [ ] **Step 3: Criar o .gitignore**

Criar `.gitignore` com este conteúdo:

````text
# Binários e saídas de teste
*.test
*.out
/coverage*

# Sistema
.DS_Store

# Inputs de fuzz que falharam ficam em testdata/fuzz e SÃO versionados.
````

- [ ] **Step 4: Criar o doc.go**

Criar `doc.go` com este conteúdo:

````go
// Package tacdb is an embeddable graph database with full-text search,
// optional vector search, transactions and durable event streams.
//
// The normative specification is SPEC.md at the repository root. This
// package currently exposes the error API; the database API arrives with
// later milestones.
package tacdb
````

- [ ] **Step 5: Substituir o README.md**

Substituir o conteúdo de `README.md` com este conteúdo:

````markdown
# tacdb

Banco de grafos embarcável em Go, com busca textual, busca vetorial opcional, transações e streams duráveis, integrado à linguagem TAC.

**Estado:** em construção. O marco atual é o M1 (fundação); nada aqui está pronto para uso.

## Documentos

- [SPEC.md](SPEC.md): especificação normativa (versão 1.3).
- [FORMAT.md](FORMAT.md): formato físico G2, como implementado.
- [COMPATIBILITY.md](COMPATIBILITY.md): matriz de compatibilidade e decisões próprias.
- [AUDIT-REGRESSIONS.md](AUDIT-REGRESSIONS.md): rastreabilidade dos itens AUD, REV e R13.

## Desenvolvimento

Requer Go 1.27. O `go.mod` fixa o toolchain 1.27.3; com `GOTOOLCHAIN=auto` (o padrão) ele é baixado na primeira execução.

```sh
go test ./...
CGO_ENABLED=1 go test -race ./...
go test ./internal/codec -fuzz=FuzzValue -fuzztime=60s
```

O código de produção compila com `CGO_ENABLED=0`.
````

- [ ] **Step 6: Verificar que o módulo compila**

Run: `go build ./... && go vet ./... && go version`

Expected: nenhuma saída dos dois primeiros comandos e `go version go1.27.3 ...` (na primeira vez o Go baixa o toolchain e imprime `go: downloading go1.27.3`).

- [ ] **Step 7: Commit**

```bash
git add go.mod .gitattributes .gitignore doc.go README.md
git commit -m "build: criar o módulo github.com/TacFlow/tacdb"
```


### Task 2: Códigos e tipo Error (internal/errs)

**Files:**
- Create: `internal/errs/codes.go`
- Create: `internal/errs/errs.go`
- Test: `internal/errs/errs_test.go`

- [ ] **Step 1: Escrever o teste que falha**

Criar `internal/errs/errs_test.go` com este conteúdo:

````go
package errs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
)

func TestAllCodesAreUniqueAndWellFormed(t *testing.T) {
	if got, want := len(AllCodes), 77; got != want {
		t.Fatalf("len(AllCodes) = %d, want %d (SPEC §6.1)", got, want)
	}
	shape := regexp.MustCompile(`^[A-Z][A-Z_]*[A-Z]$`)
	seen := make(map[Code]bool)
	for _, c := range AllCodes {
		if seen[c] {
			t.Errorf("duplicate code %s", c)
		}
		seen[c] = true
		if !shape.MatchString(string(c)) {
			t.Errorf("code %q is not UPPER_SNAKE_CASE", c)
		}
	}
}

func TestIsMatchesByCode(t *testing.T) {
	err := fmt.Errorf("outer: %w", New(CodeCorrupt, "btree.descend", "cycle"))
	if !errors.Is(err, Sentinel(CodeCorrupt)) {
		t.Fatal("errors.Is did not match a CORRUPT error against the CORRUPT sentinel")
	}
	if errors.Is(err, Sentinel(CodeIO)) {
		t.Fatal("errors.Is matched a CORRUPT error against the IO sentinel")
	}
	if errors.Is(err, (*Error)(nil)) {
		t.Fatal("errors.Is matched a nil *Error")
	}
}

func TestUnwrapKeepsCause(t *testing.T) {
	err := Wrap(CodeCanceled, "governor.reserve", context.Canceled, "wait canceled")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cause is not reachable through errors.Is")
	}
	if CodeOf(fmt.Errorf("x: %w", err)) != CodeCanceled {
		t.Fatal("CodeOf did not find the wrapped code")
	}
	if CodeOf(errors.New("plain")) != "" {
		t.Fatal(`CodeOf of a plain error must be ""`)
	}
}

func TestErrorStringIncludesSortedDetails(t *testing.T) {
	err := New(CodeResourceLimit, "governor.reserve", "budget exhausted").
		With("requested", 4096).With("available", 0).With("limit", 1024)
	want := "tacdb: RESOURCE_LIMIT: governor.reserve: budget exhausted " +
		"(available=0, limit=1024, requested=4096)"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestWithNeverModifiesASentinel(t *testing.T) {
	s := Sentinel(CodeLimitExceeded)
	e := s.With("limit", 1)
	if s.Details != nil {
		t.Fatal("With modified the sentinel")
	}
	if e == s || e.Details["limit"] != 1 || !errors.Is(e, s) {
		t.Fatalf("With on a sentinel = %#v", e)
	}
}

func TestErrorStringMinimal(t *testing.T) {
	if got, want := Sentinel(CodeBusy).Error(), "tacdb: BUSY"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	err := Wrap(CodeIO, "vfs.write", errors.New("disk on fire"), "")
	if got, want := err.Error(), "tacdb: IO: vfs.write: disk on fire"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
````

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/errs`

Expected: FAIL de compilação: `no non-test Go files` ou `undefined: AllCodes`.

- [ ] **Step 3: Escrever os códigos**

Criar `internal/errs/codes.go` com este conteúdo:

````go
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
````

- [ ] **Step 4: Escrever o tipo Error**

Criar `internal/errs/errs.go` com este conteúdo:

````go
// Package errs defines the Error type and the stable error codes shared by
// every tacdb package. The public package tacdb re-exports them.
package errs

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Location points into a query source (SPEC §6.1).
type Location struct {
	Start, End   int // byte offsets
	Line, Column int // 1-based
}

// Error is the error type returned by tacdb.
//
// errors.Is matches two *Error values by Code, so a sentinel such as
// tacdb.ErrCorrupt matches every CORRUPT error. errors.As gives access to
// the details.
type Error struct {
	Code        Code
	Op          string
	Message     string
	Cause       error
	Retryable   bool
	CommitToken *[16]byte
	Location    *Location
	// Details carries named numeric facts whose keys are stable per code
	// (SPEC §6.1). Durations are in nanoseconds. Keys in use:
	//
	//	LIMIT_EXCEEDED  limit, plus one of depth, dimensions, bytes, elements
	//	RESOURCE_LIMIT  requested, available, limit
	//	NO_SPACE        capacity (in-memory file system only)
	Details map[string]int64

	sentinel bool
}

// New returns an error with the given code, operation and message.
func New(code Code, op, msg string) *Error {
	return &Error{Code: code, Op: op, Message: msg}
}

// Newf is New with a formatted message.
func Newf(code Code, op, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Message: fmt.Sprintf(format, args...)}
}

// Wrap returns an error with the given code that wraps cause.
func Wrap(code Code, op string, cause error, msg string) *Error {
	return &Error{Code: code, Op: op, Message: msg, Cause: cause}
}

// Sentinel returns a bare error carrying only a code, for use with
// errors.Is. With never modifies a sentinel.
func Sentinel(code Code) *Error {
	return &Error{Code: code, sentinel: true}
}

// With sets a detail and returns e, for chaining at construction time. On
// a sentinel it returns a modified copy instead, so that shared sentinels
// never change.
func (e *Error) With(key string, v int64) *Error {
	if e.sentinel {
		c := *e
		c.sentinel = false
		c.Details = nil
		e = &c
	}
	if e.Details == nil {
		e.Details = make(map[string]int64)
	}
	e.Details[key] = v
	return e
}

// Error formats the error as "tacdb: CODE: op: message (k=v, ...): cause".
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("tacdb: ")
	b.WriteString(string(e.Code))
	if e.Op != "" {
		b.WriteString(": ")
		b.WriteString(e.Op)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if len(e.Details) > 0 {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" (")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%d", k, e.Details[k])
		}
		b.WriteString(")")
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Cause }

// Is reports whether target is an *Error with the same code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t != nil && t.Code == e.Code
}

// CodeOf returns the code of the first *Error in err's chain, or "" if
// there is none.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
````

- [ ] **Step 5: Rodar e ver passar**

Run: `go test ./internal/errs`

Expected: `ok  github.com/TacFlow/tacdb/internal/errs`.

- [ ] **Step 6: Commit**

```bash
git add internal/errs
git commit -m "feat(errs): códigos estáveis da SPEC §6.1 e Error com Details"
```


### Task 3: API pública de erros (raiz tacdb)

**Files:**
- Create: `errors.go`
- Test: `errors_test.go`

- [ ] **Step 1: Escrever o teste que falha**

Criar `errors_test.go` com este conteúdo:

````go
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
````

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test .`

Expected: FAIL de compilação: `undefined: CodeClosed` (e demais constantes).

- [ ] **Step 3: Escrever a API pública**

Criar `errors.go` com este conteúdo:

````go
// Error API of tacdb (SPEC §6.1). Codes and Error live in internal/errs so
// that internal packages can return them without importing this package.

package tacdb

import "github.com/TacFlow/tacdb/internal/errs"

// Code is a stable error code. Codes never change once released.
type Code = errs.Code

// Error is the error type returned by tacdb. errors.Is matches by Code, so
// errors.Is(err, ErrCorrupt) reports whether err is a CORRUPT error.
type Error = errs.Error

// Location points into a query source.
type Location = errs.Location

// Lifecycle codes.
const (
	CodeClosed              = errs.CodeClosed
	CodeBusy                = errs.CodeBusy
	CodeTxClosed            = errs.CodeTxClosed
	CodeTxInUse             = errs.CodeTxInUse
	CodeReadOnly            = errs.CodeReadOnly
	CodeDBBusy              = errs.CodeDBBusy
	CodeBusyTimeout         = errs.CodeBusyTimeout
	CodeWriterQueueFull     = errs.CodeWriterQueueFull
	CodeTooManyTransactions = errs.CodeTooManyTransactions
	CodeSnapshotExpired     = errs.CodeSnapshotExpired
	CodeWriteTxTimeout      = errs.CodeWriteTxTimeout
	CodeShuttingDown        = errs.CodeShuttingDown
)

// Input codes.
const (
	CodeInvalidArgument = errs.CodeInvalidArgument
	CodeTypeMismatch    = errs.CodeTypeMismatch
	CodeLimitExceeded   = errs.CodeLimitExceeded
	CodeNotFound        = errs.CodeNotFound
	CodeDuplicateKey    = errs.CodeDuplicateKey
)

// Query codes.
const (
	CodeQueryLex               = errs.CodeQueryLex
	CodeQueryParse             = errs.CodeQueryParse
	CodeQuerySemantic          = errs.CodeQuerySemantic
	CodeQueryUnsupported       = errs.CodeQueryUnsupported
	CodeQueryWriteRequiresExec = errs.CodeQueryWriteRequiresExec
	CodeParamMissing           = errs.CodeParamMissing
	CodeParamType              = errs.CodeParamType
)

// Persistence codes.
const (
	CodeLocked            = errs.CodeLocked
	CodeIO                = errs.CodeIO
	CodeNoSpace           = errs.CodeNoSpace
	CodeCorrupt           = errs.CodeCorrupt
	CodeUnsupportedFormat = errs.CodeUnsupportedFormat
	CodeWALLimit          = errs.CodeWALLimit
	CodeCommitUnknown     = errs.CodeCommitUnknown
	CodeCommitPending     = errs.CodeCommitPending
	CodeCommitQueueFull   = errs.CodeCommitQueueFull
	CodeRecoveryRequired  = errs.CodeRecoveryRequired
)

// Backup codes.
const (
	CodeBackupLag         = errs.CodeBackupLag
	CodeBackupInvalid     = errs.CodeBackupInvalid
	CodeDestinationExists = errs.CodeDestinationExists
)

// Indexes codes.
const (
	CodeIndexNotFound        = errs.CodeIndexNotFound
	CodeIndexExistsDifferent = errs.CodeIndexExistsDifferent
	CodeInvalidVector        = errs.CodeInvalidVector
	CodeVectorDisabled       = errs.CodeVectorDisabled
	CodeVectorKeyMismatch    = errs.CodeVectorKeyMismatch
	CodeIndexInvalid         = errs.CodeIndexInvalid
	CodeIndexInconsistent    = errs.CodeIndexInconsistent
	CodeIndexBuilding        = errs.CodeIndexBuilding
	CodeIndexBuildFailed     = errs.CodeIndexBuildFailed
	CodeUniqueViolation      = errs.CodeUniqueViolation
)

// Events codes.
const (
	CodeReservedStream   = errs.CodeReservedStream
	CodeCursorExpired    = errs.CodeCursorExpired
	CodeOffsetRegression = errs.CodeOffsetRegression
	CodeInvalidSequence  = errs.CodeInvalidSequence
)

// Execution codes.
const (
	CodeCanceled        = errs.CodeCanceled
	CodeDeadline        = errs.CodeDeadline
	CodeResourceLimit   = errs.CodeResourceLimit
	CodeQueryIncomplete = errs.CodeQueryIncomplete
)

// Cluster and IPC codes.
const (
	CodeNotLeader             = errs.CodeNotLeader
	CodeNoQuorum              = errs.CodeNoQuorum
	CodeLeaderChanged         = errs.CodeLeaderChanged
	CodeReplicaLag            = errs.CodeReplicaLag
	CodeSessionExpired        = errs.CodeSessionExpired
	CodeCursorLost            = errs.CodeCursorLost
	CodeClusterModeRequired   = errs.CodeClusterModeRequired
	CodeVersionIncompatible   = errs.CodeVersionIncompatible
	CodeInvalidDurabilityMode = errs.CodeInvalidDurabilityMode
	CodeRequestConflict       = errs.CodeRequestConflict
	CodeRequestOutOfOrder     = errs.CodeRequestOutOfOrder
	CodeResultExpired         = errs.CodeResultExpired
	CodeRequestAlreadyApplied = errs.CodeRequestAlreadyApplied
)

// Security codes.
const (
	CodeUnauthenticated     = errs.CodeUnauthenticated
	CodePermissionDenied    = errs.CodePermissionDenied
	CodePolicyViolation     = errs.CodePolicyViolation
	CodeClassifiedValue     = errs.CodeClassifiedValue
	CodeQuotaExceeded       = errs.CodeQuotaExceeded
	CodeSignatureRequired   = errs.CodeSignatureRequired
	CodeSignatureInvalid    = errs.CodeSignatureInvalid
	CodeUnsafePath          = errs.CodeUnsafePath
	CodeInsecurePermissions = errs.CodeInsecurePermissions
)

// Sentinels for lifecycle codes, for use with errors.Is.
var (
	ErrClosed              = errs.Sentinel(CodeClosed)
	ErrBusy                = errs.Sentinel(CodeBusy)
	ErrTxClosed            = errs.Sentinel(CodeTxClosed)
	ErrTxInUse             = errs.Sentinel(CodeTxInUse)
	ErrReadOnly            = errs.Sentinel(CodeReadOnly)
	ErrDBBusy              = errs.Sentinel(CodeDBBusy)
	ErrBusyTimeout         = errs.Sentinel(CodeBusyTimeout)
	ErrWriterQueueFull     = errs.Sentinel(CodeWriterQueueFull)
	ErrTooManyTransactions = errs.Sentinel(CodeTooManyTransactions)
	ErrSnapshotExpired     = errs.Sentinel(CodeSnapshotExpired)
	ErrWriteTxTimeout      = errs.Sentinel(CodeWriteTxTimeout)
	ErrShuttingDown        = errs.Sentinel(CodeShuttingDown)
)

// Sentinels for input codes, for use with errors.Is.
var (
	ErrInvalidArgument = errs.Sentinel(CodeInvalidArgument)
	ErrTypeMismatch    = errs.Sentinel(CodeTypeMismatch)
	ErrLimitExceeded   = errs.Sentinel(CodeLimitExceeded)
	ErrNotFound        = errs.Sentinel(CodeNotFound)
	ErrDuplicateKey    = errs.Sentinel(CodeDuplicateKey)
)

// Sentinels for query codes, for use with errors.Is.
var (
	ErrQueryLex               = errs.Sentinel(CodeQueryLex)
	ErrQueryParse             = errs.Sentinel(CodeQueryParse)
	ErrQuerySemantic          = errs.Sentinel(CodeQuerySemantic)
	ErrQueryUnsupported       = errs.Sentinel(CodeQueryUnsupported)
	ErrQueryWriteRequiresExec = errs.Sentinel(CodeQueryWriteRequiresExec)
	ErrParamMissing           = errs.Sentinel(CodeParamMissing)
	ErrParamType              = errs.Sentinel(CodeParamType)
)

// Sentinels for persistence codes, for use with errors.Is.
var (
	ErrLocked            = errs.Sentinel(CodeLocked)
	ErrIO                = errs.Sentinel(CodeIO)
	ErrNoSpace           = errs.Sentinel(CodeNoSpace)
	ErrCorrupt           = errs.Sentinel(CodeCorrupt)
	ErrUnsupportedFormat = errs.Sentinel(CodeUnsupportedFormat)
	ErrWALLimit          = errs.Sentinel(CodeWALLimit)
	ErrCommitUnknown     = errs.Sentinel(CodeCommitUnknown)
	ErrCommitPending     = errs.Sentinel(CodeCommitPending)
	ErrCommitQueueFull   = errs.Sentinel(CodeCommitQueueFull)
	ErrRecoveryRequired  = errs.Sentinel(CodeRecoveryRequired)
)

// Sentinels for backup codes, for use with errors.Is.
var (
	ErrBackupLag         = errs.Sentinel(CodeBackupLag)
	ErrBackupInvalid     = errs.Sentinel(CodeBackupInvalid)
	ErrDestinationExists = errs.Sentinel(CodeDestinationExists)
)

// Sentinels for indexes codes, for use with errors.Is.
var (
	ErrIndexNotFound        = errs.Sentinel(CodeIndexNotFound)
	ErrIndexExistsDifferent = errs.Sentinel(CodeIndexExistsDifferent)
	ErrInvalidVector        = errs.Sentinel(CodeInvalidVector)
	ErrVectorDisabled       = errs.Sentinel(CodeVectorDisabled)
	ErrVectorKeyMismatch    = errs.Sentinel(CodeVectorKeyMismatch)
	ErrIndexInvalid         = errs.Sentinel(CodeIndexInvalid)
	ErrIndexInconsistent    = errs.Sentinel(CodeIndexInconsistent)
	ErrIndexBuilding        = errs.Sentinel(CodeIndexBuilding)
	ErrIndexBuildFailed     = errs.Sentinel(CodeIndexBuildFailed)
	ErrUniqueViolation      = errs.Sentinel(CodeUniqueViolation)
)

// Sentinels for events codes, for use with errors.Is.
var (
	ErrReservedStream   = errs.Sentinel(CodeReservedStream)
	ErrCursorExpired    = errs.Sentinel(CodeCursorExpired)
	ErrOffsetRegression = errs.Sentinel(CodeOffsetRegression)
	ErrInvalidSequence  = errs.Sentinel(CodeInvalidSequence)
)

// Sentinels for execution codes, for use with errors.Is.
var (
	ErrCanceled        = errs.Sentinel(CodeCanceled)
	ErrDeadline        = errs.Sentinel(CodeDeadline)
	ErrResourceLimit   = errs.Sentinel(CodeResourceLimit)
	ErrQueryIncomplete = errs.Sentinel(CodeQueryIncomplete)
)

// Sentinels for cluster and ipc codes, for use with errors.Is.
var (
	ErrNotLeader             = errs.Sentinel(CodeNotLeader)
	ErrNoQuorum              = errs.Sentinel(CodeNoQuorum)
	ErrLeaderChanged         = errs.Sentinel(CodeLeaderChanged)
	ErrReplicaLag            = errs.Sentinel(CodeReplicaLag)
	ErrSessionExpired        = errs.Sentinel(CodeSessionExpired)
	ErrCursorLost            = errs.Sentinel(CodeCursorLost)
	ErrClusterModeRequired   = errs.Sentinel(CodeClusterModeRequired)
	ErrVersionIncompatible   = errs.Sentinel(CodeVersionIncompatible)
	ErrInvalidDurabilityMode = errs.Sentinel(CodeInvalidDurabilityMode)
	ErrRequestConflict       = errs.Sentinel(CodeRequestConflict)
	ErrRequestOutOfOrder     = errs.Sentinel(CodeRequestOutOfOrder)
	ErrResultExpired         = errs.Sentinel(CodeResultExpired)
	ErrRequestAlreadyApplied = errs.Sentinel(CodeRequestAlreadyApplied)
)

// Sentinels for security codes, for use with errors.Is.
var (
	ErrUnauthenticated     = errs.Sentinel(CodeUnauthenticated)
	ErrPermissionDenied    = errs.Sentinel(CodePermissionDenied)
	ErrPolicyViolation     = errs.Sentinel(CodePolicyViolation)
	ErrClassifiedValue     = errs.Sentinel(CodeClassifiedValue)
	ErrQuotaExceeded       = errs.Sentinel(CodeQuotaExceeded)
	ErrSignatureRequired   = errs.Sentinel(CodeSignatureRequired)
	ErrSignatureInvalid    = errs.Sentinel(CodeSignatureInvalid)
	ErrUnsafePath          = errs.Sentinel(CodeUnsafePath)
	ErrInsecurePermissions = errs.Sentinel(CodeInsecurePermissions)
)
````

- [ ] **Step 4: Rodar e ver passar**

Run: `go test .`

Expected: `ok  github.com/TacFlow/tacdb`.

- [ ] **Step 5: Commit**

```bash
git add errors.go errors_test.go
git commit -m "feat: API pública de erros com constantes e sentinelas"
```


### Task 4: Tipo Value, construtores e acessores

**Files:**
- Create: `value/value.go`
- Test: `value/value_test.go`

- [ ] **Step 1: Escrever o teste que falha**

Criar `value/value_test.go` com este conteúdo:

````go
package value_test

import (
	"errors"
	"math"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

func TestZeroValueIsNull(t *testing.T) {
	var v value.Value
	if !v.IsNull() || v.Kind() != value.KindNull {
		t.Fatalf("zero Value is %v, want Null", v.Kind())
	}
}

func TestScalarAccessors(t *testing.T) {
	if b, ok := value.Bool(true).AsBool(); !ok || !b {
		t.Error("Bool(true).AsBool() failed")
	}
	if i, ok := value.Int(math.MinInt64).AsInt(); !ok || i != math.MinInt64 {
		t.Error("Int(MinInt64) did not round-trip")
	}
	negZero := math.Copysign(0, -1)
	if f, ok := value.Float(negZero).AsFloat(); !ok || math.Float64bits(f) != math.Float64bits(negZero) {
		t.Error("Float(-0) lost the sign")
	}
	payload := math.Float64frombits(0x7ff8000000000001)
	if f, _ := value.Float(payload).AsFloat(); math.Float64bits(f) != 0x7ff8000000000001 {
		t.Error("Float lost the NaN payload")
	}
	if s, ok := value.String("ação").AsString(); !ok || s != "ação" {
		t.Error("String did not round-trip")
	}
	if _, ok := value.Int(1).AsString(); ok {
		t.Error("AsString on an Int reported ok")
	}
}

// VAL-04 (input part): mutating the caller's slices after construction
// must not change the value.
func TestConstructorsCopyInputs(t *testing.T) {
	b := []byte{1, 2, 3}
	bv := value.Bytes(b)
	b[0] = 9
	if got, _ := bv.AsBytes(); got[0] != 1 {
		t.Error("Bytes shares memory with the caller")
	}
	got, _ := bv.AsBytes()
	got[1] = 9
	if again, _ := bv.AsBytes(); again[1] != 2 {
		t.Error("AsBytes exposes internal memory")
	}

	vec := []float32{1, 2}
	vv := value.Vector(vec)
	vec[0] = 9
	if vv.VectorAt(0) != 1 {
		t.Error("Vector shares memory with the caller")
	}

	items := []value.Value{value.Int(1), value.Int(2)}
	lv := value.List(items...)
	items[0] = value.Int(9)
	if i, _ := lv.Index(0).AsInt(); i != 1 {
		t.Error("List shares memory with the caller")
	}

	entries := []value.Entry{{Key: "a", Value: value.Int(1)}}
	mv, err := value.MapFromEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].Key = "z"
	if k, _ := mv.EntryAt(0); k != "a" {
		t.Error("MapFromEntries shares memory with the caller")
	}
}

func TestMapsAreSortedByKeyBytes(t *testing.T) {
	m := value.MapOf(map[string]value.Value{"b": value.Int(2), "a": value.Int(1), "B": value.Int(0)})
	var keys []string
	for i := 0; i < m.Len(); i++ {
		k, _ := m.EntryAt(i)
		keys = append(keys, k)
	}
	want := []string{"B", "a", "b"}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %q, want %q", keys, want)
		}
	}
	if v, ok := m.Lookup("a"); !ok || !value.Identical(v, value.Int(1)) {
		t.Error("Lookup(a) failed")
	}
	if _, ok := m.Lookup("c"); ok {
		t.Error("Lookup(c) found a missing key")
	}
}

func TestMapFromEntriesRejectsDuplicates(t *testing.T) {
	_, err := value.MapFromEntries([]value.Entry{{Key: "k"}, {Key: "k"}})
	if !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
}

func TestMakeMapRequiresIncreasingKeys(t *testing.T) {
	keys := []string{"b", "a"}
	_, err := value.MakeMap(2, func(i int) (value.Entry, error) {
		return value.Entry{Key: keys[i]}, nil
	})
	if !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
}

func TestIdenticalDistinguishesBits(t *testing.T) {
	cases := []struct {
		a, b value.Value
		want bool
	}{
		{value.Int(1), value.Float(1), false},
		{value.Float(0), value.Float(math.Copysign(0, -1)), false},
		{value.Float(math.NaN()), value.Float(math.NaN()), true},
		{value.String("x"), value.Bytes([]byte("x")), false},
		{value.List(value.Int(1)), value.List(value.Int(1)), true},
		{value.List(value.Int(1)), value.List(value.Int(1), value.Null()), false},
		{value.Vector([]float32{1}), value.Vector([]float32{1}), true},
	}
	for i, c := range cases {
		if got := value.Identical(c.a, c.b); got != c.want {
			t.Errorf("case %d: Identical(%v, %v) = %v, want %v", i, c.a, c.b, got, c.want)
		}
	}
}

func TestDebugString(t *testing.T) {
	v := value.MapOf(map[string]value.Value{
		"l": value.List(value.Null(), value.Bool(true), value.Bytes([]byte{0, 255})),
		"v": value.Vector([]float32{1, 0.5}),
	})
	want := `{"l": [null, true, 0x00ff], "v": vector[1 0.5]}`
	if got := v.String(); got != want {
		t.Fatalf("String() = %s, want %s", got, want)
	}
}
````

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./value`

Expected: FAIL de compilação: `no non-test Go files in .../value`.

- [ ] **Step 3: Escrever o tipo Value**

Criar `value/value.go` com este conteúdo:

````go
// Package value defines the typed values that tacdb stores in properties
// (SPEC §5). A Value is immutable: constructors copy their inputs and
// accessors never expose memory the caller could mutate.
package value

import (
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
)

// Kind is the type of a Value. Its numeric value is the G2 tag.
type Kind uint8

// Kinds, in tag order.
const (
	KindNull   Kind = 0x00
	KindBool   Kind = 0x01
	KindInt    Kind = 0x02
	KindFloat  Kind = 0x03
	KindString Kind = 0x04
	KindBytes  Kind = 0x05
	KindVector Kind = 0x06
	KindList   Kind = 0x07
	KindMap    Kind = 0x08
)

var kindNames = [...]string{"Null", "Bool", "Int", "Float", "String", "Bytes", "Vector", "List", "Map"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Value is an immutable typed value. The zero Value is Null.
//
// Values cannot be compared with == or used as map keys; use Compare or
// Identical.
type Value struct {
	_    [0]func() // makes == a compile error instead of a runtime panic
	kind Kind
	num  uint64 // Bool (0 or 1), Int (two's complement), Float (IEEE-754 bits)
	str  string // String and Bytes payload; a string keeps Bytes immutable
	ref  any    // []float32 (Vector), []Value (List) or []Entry (Map)
}

// Entry is a key/value pair of a Map.
type Entry struct {
	Key   string
	Value Value
}

// Null returns the Null value.
func Null() Value { return Value{} }

// Bool returns a Bool value.
func Bool(b bool) Value {
	if b {
		return Value{kind: KindBool, num: 1}
	}
	return Value{kind: KindBool}
}

// Int returns an Int value.
func Int(i int64) Value { return Value{kind: KindInt, num: uint64(i)} }

// Float returns a Float value. NaN payloads, infinities and the sign of
// zero are preserved.
func Float(f float64) Value { return Value{kind: KindFloat, num: math.Float64bits(f)} }

// String returns a String value. Validate checks that s is valid UTF-8.
func String(s string) Value { return Value{kind: KindString, str: s} }

// Bytes returns a Bytes value holding a copy of b.
func Bytes(b []byte) Value { return Value{kind: KindBytes, str: string(b)} }

// Vector returns a Vector value holding a copy of v.
func Vector(v []float32) Value {
	return Value{kind: KindVector, ref: append([]float32(nil), v...)}
}

// List returns a List value holding a copy of items.
func List(items ...Value) Value {
	return Value{kind: KindList, ref: append([]Value(nil), items...)}
}

// MapOf returns a Map value with the entries of m, sorted by key bytes.
func MapOf(m map[string]Value) Value {
	entries := make([]Entry, 0, len(m))
	for k, v := range m {
		entries = append(entries, Entry{Key: k, Value: v})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return Value{kind: KindMap, ref: entries}
}

// MapFromEntries returns a Map value with a sorted copy of entries. A
// duplicate key is an INVALID_ARGUMENT error.
func MapFromEntries(entries []Entry) (Value, error) {
	sorted := append([]Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Key == sorted[i-1].Key {
			return Value{}, errs.Newf(errs.CodeInvalidArgument, "value.MapFromEntries",
				"duplicate key at entries %d and %d", i-1, i)
		}
	}
	return Value{kind: KindMap, ref: sorted}, nil
}

// MakeVector returns a Vector of n components produced by at, without an
// intermediate copy.
func MakeVector(n int, at func(i int) float32) Value {
	v := make([]float32, n)
	for i := range v {
		v[i] = at(i)
	}
	return Value{kind: KindVector, ref: v}
}

// MakeList returns a List of n items produced by at, stopping at the first
// error.
func MakeList(n int, at func(i int) (Value, error)) (Value, error) {
	items := make([]Value, n)
	for i := range items {
		item, err := at(i)
		if err != nil {
			return Value{}, err
		}
		items[i] = item
	}
	return Value{kind: KindList, ref: items}, nil
}

// MakeMap returns a Map of n entries produced by at. Keys must arrive in
// strictly increasing byte order; otherwise it is an INVALID_ARGUMENT
// error.
func MakeMap(n int, at func(i int) (Entry, error)) (Value, error) {
	entries := make([]Entry, n)
	for i := range entries {
		e, err := at(i)
		if err != nil {
			return Value{}, err
		}
		if i > 0 && e.Key <= entries[i-1].Key {
			return Value{}, errs.Newf(errs.CodeInvalidArgument, "value.MakeMap",
				"entry %d is not after entry %d in key order", i, i-1)
		}
		entries[i] = e
	}
	return Value{kind: KindMap, ref: entries}, nil
}

// Kind returns the type of v.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether v is Null.
func (v Value) IsNull() bool { return v.kind == KindNull }

// AsBool returns the boolean and true if v is a Bool.
func (v Value) AsBool() (bool, bool) { return v.num == 1, v.kind == KindBool }

// AsInt returns the integer and true if v is an Int.
func (v Value) AsInt() (int64, bool) { return int64(v.num), v.kind == KindInt }

// AsFloat returns the float and true if v is a Float.
func (v Value) AsFloat() (float64, bool) {
	return math.Float64frombits(v.num), v.kind == KindFloat
}

// AsString returns the string and true if v is a String.
func (v Value) AsString() (string, bool) { return v.str, v.kind == KindString }

// AsBytes returns a copy of the bytes and true if v is Bytes.
func (v Value) AsBytes() ([]byte, bool) {
	if v.kind != KindBytes {
		return nil, false
	}
	return []byte(v.str), true
}

// AppendBytes appends the payload of a String or Bytes value to dst.
func (v Value) AppendBytes(dst []byte) []byte { return append(dst, v.str...) }

// Len returns the byte length of a String or Bytes, the dimension of a
// Vector, the number of items of a List or of entries of a Map, and 0 for
// scalars.
func (v Value) Len() int {
	switch v.kind {
	case KindString, KindBytes:
		return len(v.str)
	case KindVector:
		return len(v.ref.([]float32))
	case KindList:
		return len(v.ref.([]Value))
	case KindMap:
		return len(v.ref.([]Entry))
	}
	return 0
}

// VectorAt returns component i of a Vector.
func (v Value) VectorAt(i int) float32 { return v.ref.([]float32)[i] }

// AppendVector appends the components of a Vector to dst.
func (v Value) AppendVector(dst []float32) []float32 {
	return append(dst, v.ref.([]float32)...)
}

// Index returns item i of a List.
func (v Value) Index(i int) Value { return v.ref.([]Value)[i] }

// EntryAt returns entry i of a Map, in key order.
func (v Value) EntryAt(i int) (string, Value) {
	e := v.ref.([]Entry)[i]
	return e.Key, e.Value
}

// Lookup returns the value of key in a Map.
func (v Value) Lookup(key string) (Value, bool) {
	if v.kind != KindMap {
		return Value{}, false
	}
	entries := v.ref.([]Entry)
	i := sort.Search(len(entries), func(i int) bool { return entries[i].Key >= key })
	if i < len(entries) && entries[i].Key == key {
		return entries[i].Value, true
	}
	return Value{}, false
}

// Identical reports whether a and b have the same kind and the same bits,
// recursively. Unlike Compare, Identical distinguishes Int(1) from
// Float(1), -0 from +0 and NaN payloads.
func Identical(a, b Value) bool {
	if a.kind != b.kind || a.num != b.num || a.str != b.str {
		return false
	}
	switch a.kind {
	case KindVector:
		x, y := a.ref.([]float32), b.ref.([]float32)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(y[i]) {
				return false
			}
		}
	case KindList:
		x, y := a.ref.([]Value), b.ref.([]Value)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Identical(x[i], y[i]) {
				return false
			}
		}
	case KindMap:
		x, y := a.ref.([]Entry), b.ref.([]Entry)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i].Key != y[i].Key || !Identical(x[i].Value, y[i].Value) {
				return false
			}
		}
	}
	return true
}

// String returns a debugging representation of v. It may contain user
// data, so it must not be used in logs or error messages.
func (v Value) String() string {
	var b strings.Builder
	v.format(&b)
	return b.String()
}

func (v Value) format(b *strings.Builder) {
	switch v.kind {
	case KindNull:
		b.WriteString("null")
	case KindBool:
		b.WriteString(strconv.FormatBool(v.num == 1))
	case KindInt:
		b.WriteString(strconv.FormatInt(int64(v.num), 10))
	case KindFloat:
		b.WriteString(strconv.FormatFloat(math.Float64frombits(v.num), 'g', -1, 64))
	case KindString:
		b.WriteString(strconv.Quote(v.str))
	case KindBytes:
		b.WriteString("0x")
		b.WriteString(hex.EncodeToString([]byte(v.str)))
	case KindVector:
		b.WriteString("vector[")
		for i, c := range v.ref.([]float32) {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strconv.FormatFloat(float64(c), 'g', -1, 32))
		}
		b.WriteByte(']')
	case KindList:
		b.WriteByte('[')
		for i, item := range v.ref.([]Value) {
			if i > 0 {
				b.WriteString(", ")
			}
			item.format(b)
		}
		b.WriteByte(']')
	case KindMap:
		b.WriteByte('{')
		for i, e := range v.ref.([]Entry) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.Quote(e.Key))
			b.WriteString(": ")
			e.Value.format(b)
		}
		b.WriteByte('}')
	}
}
````

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./value`

Expected: `ok  github.com/TacFlow/tacdb/value`.

- [ ] **Step 5: Commit**

```bash
git add value
git commit -m "feat(value): tipo Value imutável com os 9 tipos da SPEC §5.1"
```


### Task 5: Ordem total (Compare) e gerador de valores de teste

**Files:**
- Create: `value/compare.go`
- Create: `internal/valuetest/gen.go`
- Test: `value/compare_test.go`

- [ ] **Step 1: Escrever o gerador de valores de teste**

Criar `internal/valuetest/gen.go` com este conteúdo:

````go
// Package valuetest generates values for tests. It favors edge cases:
// NaN payloads, signed zeros, integers around 2^53, multibyte UTF-8, NUL
// bytes and nesting.
package valuetest

import (
	"math"
	"math/rand/v2"

	"github.com/TacFlow/tacdb/value"
)

var specialFloats = []float64{
	0, math.Copysign(0, -1), 1, -1, 0.5, math.Inf(1), math.Inf(-1), math.NaN(),
	math.Float64frombits(0x7ff8000000000001), // NaN with payload
	math.MaxFloat64, math.SmallestNonzeroFloat64,
	1 << 53, 1<<53 + 2, -(1 << 53), 0x1p63, -0x1p63, 0x1p64,
}

var specialInts = []int64{
	0, 1, -1, math.MaxInt64, math.MinInt64, 1 << 53, 1<<53 + 1, -(1<<53 + 1),
	math.MaxInt32, math.MinInt32,
}

var specialStrings = []string{"", "a", "ação", "a\x00b", "日本語", "\U0001F600", "zzz"}

// Value returns a random valid value with nesting depth at most maxDepth.
func Value(r *rand.Rand, maxDepth int) value.Value {
	kinds := 7
	if maxDepth > 1 {
		kinds = 9
	}
	switch r.IntN(kinds) {
	case 0:
		return value.Null()
	case 1:
		return value.Bool(r.IntN(2) == 1)
	case 2:
		if r.IntN(2) == 0 {
			return value.Int(specialInts[r.IntN(len(specialInts))])
		}
		return value.Int(int64(r.Uint64()))
	case 3:
		if r.IntN(2) == 0 {
			return value.Float(specialFloats[r.IntN(len(specialFloats))])
		}
		return value.Float(r.NormFloat64() * 1e6)
	case 4:
		if r.IntN(2) == 0 {
			return value.String(specialStrings[r.IntN(len(specialStrings))])
		}
		return value.String(randomString(r))
	case 5:
		b := make([]byte, r.IntN(12))
		for i := range b {
			b[i] = byte(r.IntN(3)) * 0x7f // 0x00, 0x7f, 0xfe
		}
		return value.Bytes(b)
	case 6:
		v := make([]float32, 1+r.IntN(5))
		for i := range v {
			if r.IntN(4) == 0 {
				v[i] = float32(specialFloats[r.IntN(len(specialFloats))])
			} else {
				v[i] = float32(r.NormFloat64())
			}
		}
		return value.Vector(v)
	case 7:
		items := make([]value.Value, r.IntN(4))
		for i := range items {
			items[i] = Value(r, maxDepth-1)
		}
		return value.List(items...)
	default:
		m := make(map[string]value.Value)
		for range r.IntN(4) {
			m[randomString(r)] = Value(r, maxDepth-1)
		}
		return value.MapOf(m)
	}
}

func randomString(r *rand.Rand) string {
	runes := []rune{'a', 'b', 'z', 'ç', 'é', 0, '日', '\U0001F600'}
	out := make([]rune, r.IntN(6))
	for i := range out {
		out[i] = runes[r.IntN(len(runes))]
	}
	return string(out)
}
````

- [ ] **Step 2: Escrever o teste que falha**

Criar `value/compare_test.go` com este conteúdo:

````go
package value_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/TacFlow/tacdb/internal/valuetest"
	"github.com/TacFlow/tacdb/value"
)

func TestCompareFixedCases(t *testing.T) {
	nan := value.Float(math.NaN())
	cases := []struct {
		name string
		a, b value.Value
		want int
	}{
		{"family Bool<number", value.Bool(true), value.Int(-5), -1},
		{"family number<String", value.Float(math.Inf(1)), value.String(""), -1},
		{"family String<Bytes", value.String("z"), value.Bytes(nil), -1},
		{"family Bytes<Vector", value.Bytes([]byte{255}), value.Vector([]float32{-1}), -1},
		{"family Vector<List", value.Vector([]float32{9}), value.List(), -1},
		{"family List<Map", value.List(value.Int(9)), value.MapOf(nil), -1},
		{"family Map<Null", value.MapOf(nil), value.Null(), -1},
		{"Null equals Null", value.Null(), value.Null(), 0},
		{"false<true", value.Bool(false), value.Bool(true), -1},
		{"Int exact above 2^53", value.Int(1<<53 + 1), value.Float(1 << 53), 1},
		{"Int exact below 2^53", value.Float(1<<53 + 2), value.Int(1<<53 + 1), 1},
		{"Int equals Float", value.Int(3), value.Float(3), 0},
		{"Int below fraction", value.Int(2), value.Float(2.5), -1},
		{"negative fraction", value.Int(-2), value.Float(-2.5), 1},
		{"MaxInt64 below 2^63", value.Int(math.MaxInt64), value.Float(0x1p63), -1},
		{"MinInt64 equals -2^63", value.Int(math.MinInt64), value.Float(-0x1p63), 0},
		{"-Inf below MinInt64", value.Float(math.Inf(-1)), value.Int(math.MinInt64), -1},
		{"NaN after +Inf", nan, value.Float(math.Inf(1)), 1},
		{"NaN after Int", nan, value.Int(math.MaxInt64), 1},
		{"NaN equals NaN", nan, value.Float(math.Float64frombits(0x7ff8000000000001)), 0},
		{"NaN before String", nan, value.String(""), -1},
		{"-0 equals +0", value.Float(math.Copysign(0, -1)), value.Float(0), 0},
		{"String by bytes", value.String("B"), value.String("a"), -1},
		{"String prefix first", value.String("a"), value.String("a\x00"), -1},
		{"List prefix first", value.List(value.Int(1)), value.List(value.Int(1), value.Null()), -1},
		{"List element order", value.List(value.Int(2)), value.List(value.Int(1), value.Int(9)), 1},
		{"Map key first", value.MapOf(map[string]value.Value{"a": value.Int(9)}),
			value.MapOf(map[string]value.Value{"b": value.Int(0)}), -1},
		{"Map value second", value.MapOf(map[string]value.Value{"a": value.Int(1)}),
			value.MapOf(map[string]value.Value{"a": value.Int(2)}), -1},
		{"Vector NaN component last", value.Vector([]float32{float32(math.NaN())}),
			value.Vector([]float32{float32(math.Inf(1))}), 1},
	}
	for _, c := range cases {
		if got := value.Compare(c.a, c.b); got != c.want {
			t.Errorf("%s: Compare(%v, %v) = %d, want %d", c.name, c.a, c.b, got, c.want)
		}
		if got := value.Compare(c.b, c.a); got != -c.want {
			t.Errorf("%s: reversed Compare(%v, %v) = %d, want %d", c.name, c.b, c.a, got, -c.want)
		}
	}
}

// Compare must be a total preorder: reflexive, antisymmetric in sign and
// transitive.
func TestCompareIsTotalPreorder(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	vals := make([]value.Value, 300)
	for i := range vals {
		vals[i] = valuetest.Value(r, 3)
	}
	for _, a := range vals {
		if value.Compare(a, a) != 0 {
			t.Fatalf("Compare(%v, itself) != 0", a)
		}
	}
	for _, a := range vals {
		for _, b := range vals {
			ab, ba := value.Compare(a, b), value.Compare(b, a)
			if ab != -ba {
				t.Fatalf("Compare(%v,%v)=%d but Compare(%v,%v)=%d", a, b, ab, b, a, ba)
			}
		}
	}
	for i := 0; i < 20000; i++ {
		a, b, c := vals[r.IntN(len(vals))], vals[r.IntN(len(vals))], vals[r.IntN(len(vals))]
		if value.Compare(a, b) <= 0 && value.Compare(b, c) <= 0 && value.Compare(a, c) > 0 {
			t.Fatalf("not transitive: %v <= %v <= %v but %v > %v", a, b, c, a, c)
		}
	}
}
````

- [ ] **Step 3: Rodar e ver falhar**

Run: `go test ./value`

Expected: FAIL de compilação: `undefined: value.Compare`.

- [ ] **Step 4: Escrever o Compare**

Criar `value/compare.go` com este conteúdo:

````go
package value

import (
	"math"
	"strings"
)

// family orders kinds for Compare (SPEC §5.3): Bool < number < String <
// Bytes < Vector < List < Map < Null.
func family(k Kind) int {
	switch k {
	case KindBool:
		return 0
	case KindInt, KindFloat:
		return 1
	case KindString:
		return 2
	case KindBytes:
		return 3
	case KindVector:
		return 4
	case KindList:
		return 5
	case KindMap:
		return 6
	}
	return 7 // Null
}

// Compare returns -1, 0 or +1 ordering a before, equal to or after b in the
// shared total order of SPEC §5.3, used by ORDER BY, DISTINCT and
// aggregation.
//
// Int and Float compare by exact numeric value, never converting an int64
// to float64. NaN orders after every other number and equals any NaN; -0
// equals +0. Compare is a total preorder: Compare == 0 does not imply
// Identical (Int(1) and Float(1) compare equal). Null orders last, so it
// comes last in ascending order and first in descending order.
func Compare(a, b Value) int {
	fa, fb := family(a.kind), family(b.kind)
	if fa != fb {
		return cmpInt(fa, fb)
	}
	switch a.kind {
	case KindNull:
		return 0
	case KindBool:
		return cmpInt(int(a.num), int(b.num))
	case KindInt, KindFloat:
		return compareNumbers(a, b)
	case KindString, KindBytes:
		return strings.Compare(a.str, b.str)
	case KindVector:
		x, y := a.ref.([]float32), b.ref.([]float32)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := compareFloats(float64(x[i]), float64(y[i])); c != 0 {
				return c
			}
		}
		return cmpInt(len(x), len(y))
	case KindList:
		x, y := a.ref.([]Value), b.ref.([]Value)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := Compare(x[i], y[i]); c != 0 {
				return c
			}
		}
		return cmpInt(len(x), len(y))
	case KindMap:
		x, y := a.ref.([]Entry), b.ref.([]Entry)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := strings.Compare(x[i].Key, y[i].Key); c != 0 {
				return c
			}
			if c := Compare(x[i].Value, y[i].Value); c != 0 {
				return c
			}
		}
		return cmpInt(len(x), len(y))
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareNumbers(a, b Value) int {
	switch {
	case a.kind == KindInt && b.kind == KindInt:
		x, y := int64(a.num), int64(b.num)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case a.kind == KindFloat && b.kind == KindFloat:
		return compareFloats(math.Float64frombits(a.num), math.Float64frombits(b.num))
	case a.kind == KindInt:
		return compareIntFloat(int64(a.num), math.Float64frombits(b.num))
	default:
		return -compareIntFloat(int64(b.num), math.Float64frombits(a.num))
	}
}

// compareFloats orders NaN after every number and treats -0 as +0.
func compareFloats(x, y float64) int {
	xn, yn := math.IsNaN(x), math.IsNaN(y)
	switch {
	case xn && yn:
		return 0
	case xn:
		return 1
	case yn:
		return -1
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// compareIntFloat compares i and f exactly.
func compareIntFloat(i int64, f float64) int {
	switch {
	case math.IsNaN(f):
		return -1
	case f >= 0x1p63:
		return -1
	case f < -0x1p63:
		return 1
	}
	// -2^63 <= f < 2^63, so the truncation fits in an int64 and is exact.
	t := int64(f)
	switch {
	case i < t:
		return -1
	case i > t:
		return 1
	}
	// i == trunc(f); the fractional part of a float64 is exact.
	frac := f - float64(t)
	switch {
	case frac > 0:
		return -1
	case frac < 0:
		return 1
	}
	return 0
}
````

- [ ] **Step 5: Rodar e ver passar**

Run: `go test ./value`

Expected: `ok  github.com/TacFlow/tacdb/value`.

- [ ] **Step 6: Commit**

```bash
git add value/compare.go value/compare_test.go internal/valuetest
git commit -m "feat(value): ordem total da SPEC §5.3 com Int e Float exatos"
```


### Task 6: Limites e validação

**Files:**
- Create: `value/limits.go`
- Test: `value/limits_test.go`

- [ ] **Step 1: Escrever o teste que falha**

Criar `value/limits_test.go` com este conteúdo:

````go
package value_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

func nest(depth int) value.Value {
	v := value.Int(1)
	for i := 1; i < depth; i++ {
		v = value.List(v)
	}
	return v
}

func TestValidateAcceptsDefaults(t *testing.T) {
	lim := value.DefaultLimits()
	if err := lim.Check(); err != nil {
		t.Fatal(err)
	}
	ok := []value.Value{
		value.Null(), value.String("a\x00ç"), nest(64),
		value.Vector(make([]float32, 4096)),
		value.MapOf(map[string]value.Value{"": value.Null(), "\x00": value.Int(1)}),
		value.MapOf(map[string]value.Value{strings.Repeat("k", 64<<10): value.Null()}),
	}
	for _, v := range ok {
		if err := value.Validate(v, lim); err != nil {
			t.Errorf("Validate(%.40s) = %v", v.String(), err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	lim := value.DefaultLimits()
	limitExceeded := errs.Sentinel(errs.CodeLimitExceeded)
	invalid := errs.Sentinel(errs.CodeInvalidArgument)
	cases := []struct {
		name string
		v    value.Value
		want error
	}{
		{"depth 65", nest(65), limitExceeded},
		{"bad UTF-8 string", value.String("\xff"), invalid},
		{"bad UTF-8 map key", value.MapOf(map[string]value.Value{"\xc3": value.Null()}), invalid},
		{"map key above 64 KiB", value.MapOf(map[string]value.Value{strings.Repeat("k", 64<<10+1): value.Null()}), limitExceeded},
		{"empty vector", value.Vector(nil), invalid},
		{"vector above 4096", value.Vector(make([]float32, 4097)), limitExceeded},
		{"nested bad string", value.List(value.Int(1), value.List(value.String("\xff"))), invalid},
	}
	for _, c := range cases {
		if err := value.Validate(c.v, lim); !errors.Is(err, c.want) {
			t.Errorf("%s: Validate = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestValidateCountsElementsAcrossLevels(t *testing.T) {
	lim := value.DefaultLimits()
	lim.MaxValueElements = 5
	inner := value.List(value.Null(), value.Null(), value.Null())  // 3 elements
	if err := value.Validate(value.List(inner), lim); err != nil { // 1 + 3 = 4
		t.Fatalf("4 elements rejected: %v", err)
	}
	err := value.Validate(value.List(inner, value.Null()), lim) // 2 + 3 = 5
	if err != nil {
		t.Fatalf("5 elements rejected: %v", err)
	}
	err = value.Validate(value.List(inner, inner), lim) // 2 + 3 + 3 = 8
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeLimitExceeded || e.Details["limit"] != 5 {
		t.Fatalf("8 elements: err = %v, want LIMIT_EXCEEDED with limit=5", err)
	}
}

func TestLimitsCheck(t *testing.T) {
	bad := value.DefaultLimits()
	bad.MaxVectorDimensions = 70000
	if err := bad.Check(); !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Fatalf("Check = %v, want INVALID_ARGUMENT", err)
	}
}
````

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./value`

Expected: FAIL de compilação: `undefined: value.DefaultLimits`.

- [ ] **Step 3: Escrever os limites**

Criar `value/limits.go` com este conteúdo:

````go
package value

import (
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/errs"
)

// Maxima fixed by the G2 format (SPEC §31.4). Limits may lower them, never
// raise them.
const (
	FormatMaxDepth       = 64
	FormatMaxValueBytes  = 16 << 20
	FormatMaxMapKeyBytes = 64 << 10
	FormatMaxVectorDims  = 65535
)

// Limits bounds the size and shape of a Value (SPEC §5.1, §24.1).
type Limits struct {
	MaxDepth            int // nesting depth; a scalar has depth 1
	MaxValueBytes       int // size of the G2 encoding
	MaxValueElements    int // List items plus Map entries, summed over all levels
	MaxMapKeyBytes      int // byte length of a key of a Map inside a value
	MaxVectorDimensions int // dimension of a Vector
}

// DefaultLimits returns the defaults of SPEC §24.1.
func DefaultLimits() Limits {
	return Limits{
		MaxDepth:            64,
		MaxValueBytes:       16 << 20,
		MaxValueElements:    1_000_000,
		MaxMapKeyBytes:      64 << 10,
		MaxVectorDimensions: 4096,
	}
}

// Check reports whether the limits are positive and within the format
// maxima.
func (l Limits) Check() error {
	bad := func(name string, v, max int) error {
		return errs.Newf(errs.CodeInvalidArgument, "value.Limits", "%s = %d, want 1..%d", name, v, max)
	}
	switch {
	case l.MaxDepth < 1 || l.MaxDepth > FormatMaxDepth:
		return bad("MaxDepth", l.MaxDepth, FormatMaxDepth)
	case l.MaxValueBytes < 1 || l.MaxValueBytes > FormatMaxValueBytes:
		return bad("MaxValueBytes", l.MaxValueBytes, FormatMaxValueBytes)
	case l.MaxValueElements < 0:
		return errs.Newf(errs.CodeInvalidArgument, "value.Limits", "MaxValueElements = %d, want >= 0", l.MaxValueElements)
	case l.MaxMapKeyBytes < 1 || l.MaxMapKeyBytes > FormatMaxMapKeyBytes:
		return bad("MaxMapKeyBytes", l.MaxMapKeyBytes, FormatMaxMapKeyBytes)
	case l.MaxVectorDimensions < 1 || l.MaxVectorDimensions > FormatMaxVectorDims:
		return bad("MaxVectorDimensions", l.MaxVectorDimensions, FormatMaxVectorDims)
	}
	return nil
}

// Validate checks v against lim: valid UTF-8 in strings and map keys,
// nesting depth, element count, map key length and vector dimension. The
// encoded size is checked by the codec, which owns the encoding.
func Validate(v Value, lim Limits) error {
	w := validator{lim: lim}
	return w.walk(v, 1)
}

type validator struct {
	lim      Limits
	elements int
}

func (w *validator) walk(v Value, depth int) error {
	const op = "value.Validate"
	if depth > w.lim.MaxDepth {
		return errs.New(errs.CodeLimitExceeded, op, "value nested too deeply").
			With("depth", int64(depth)).With("limit", int64(w.lim.MaxDepth))
	}
	switch v.kind {
	case KindString:
		if !utf8.ValidString(v.str) {
			return errs.New(errs.CodeInvalidArgument, op, "string is not valid UTF-8")
		}
	case KindVector:
		n := len(v.ref.([]float32))
		if n == 0 {
			return errs.New(errs.CodeInvalidArgument, op, "vector has no components")
		}
		if n > w.lim.MaxVectorDimensions {
			return errs.New(errs.CodeLimitExceeded, op, "vector dimension above limit").
				With("dimensions", int64(n)).With("limit", int64(w.lim.MaxVectorDimensions))
		}
	case KindList:
		items := v.ref.([]Value)
		if err := w.count(len(items)); err != nil {
			return err
		}
		for _, item := range items {
			if err := w.walk(item, depth+1); err != nil {
				return err
			}
		}
	case KindMap:
		entries := v.ref.([]Entry)
		if err := w.count(len(entries)); err != nil {
			return err
		}
		for _, e := range entries {
			if !utf8.ValidString(e.Key) {
				return errs.New(errs.CodeInvalidArgument, op, "map key is not valid UTF-8")
			}
			if len(e.Key) > w.lim.MaxMapKeyBytes {
				return errs.New(errs.CodeLimitExceeded, op, "map key too long").
					With("bytes", int64(len(e.Key))).With("limit", int64(w.lim.MaxMapKeyBytes))
			}
			if err := w.walk(e.Value, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *validator) count(n int) error {
	w.elements += n
	if w.elements > w.lim.MaxValueElements {
		return errs.New(errs.CodeLimitExceeded, "value.Validate", "too many elements").
			With("elements", int64(w.elements)).With("limit", int64(w.lim.MaxValueElements))
	}
	return nil
}
````

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./value`

Expected: `ok  github.com/TacFlow/tacdb/value`.

- [ ] **Step 5: Commit**

```bash
git add value/limits.go value/limits_test.go
git commit -m "feat(value): limites e validação da SPEC §5.1"
```


### Task 7: Uvarint mínimo

**Files:**
- Create: `internal/codec/uvarint.go`
- Test: `internal/codec/uvarint_test.go`

- [ ] **Step 1: Escrever o teste que falha**

Criar `internal/codec/uvarint_test.go` com este conteúdo:

````go
package codec

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func TestUvarintRoundTrip(t *testing.T) {
	for _, x := range []uint64{0, 1, 127, 128, 255, 1023, 1024, 16383, 16384, 1 << 24, math.MaxUint32, math.MaxUint64} {
		enc := AppendUvarint(nil, x)
		if len(enc) != UvarintLen(x) {
			t.Errorf("UvarintLen(%d) = %d, encoding has %d bytes", x, UvarintLen(x), len(enc))
		}
		got, n, err := ReadUvarint(append(enc, 0xAA)) // trailing byte is not consumed
		if err != nil || got != x || n != len(enc) {
			t.Errorf("ReadUvarint(%x) = %d, %d, %v; want %d, %d", enc, got, n, err, x, len(enc))
		}
	}
}

func TestUvarintLenBoundaries(t *testing.T) {
	cases := map[uint64]int{0: 1, 127: 1, 128: 2, 1024: 2, 16383: 2, 16384: 3, 1 << 24: 4, math.MaxUint64: 10}
	for x, want := range cases {
		if got := UvarintLen(x); got != want {
			t.Errorf("UvarintLen(%d) = %d, want %d", x, got, want)
		}
	}
}

func TestReadUvarintRejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":            nil,
		"truncated":        {0x80},
		"non-minimal zero": {0x80, 0x00},
		"non-minimal one":  {0x81, 0x00},
		"overflow":         {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02},
		"too long":         bytes.Repeat([]byte{0x80}, 11),
	}
	for name, b := range cases {
		if _, _, err := ReadUvarint(b); !errors.Is(err, errs.Sentinel(errs.CodeCorrupt)) {
			t.Errorf("%s: err = %v, want CORRUPT", name, err)
		}
	}
}
````

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/codec`

Expected: FAIL de compilação: `no non-test Go files` ou `undefined: AppendUvarint`.

- [ ] **Step 3: Escrever o uvarint**

Criar `internal/codec/uvarint.go` com este conteúdo:

````go
// Package codec implements the G2 encodings that do not depend on pages:
// minimal uvarints, the Value codec (SPEC §5.3) and composite B+tree keys
// (SPEC §7.4).
package codec

import (
	"encoding/binary"

	"github.com/TacFlow/tacdb/internal/errs"
)

// UvarintLen returns the size of the minimal uvarint encoding of x.
func UvarintLen(x uint64) int {
	n := 1
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}

// AppendUvarint appends the minimal uvarint (LEB128) encoding of x.
func AppendUvarint(dst []byte, x uint64) []byte {
	return binary.AppendUvarint(dst, x)
}

// ReadUvarint decodes a uvarint from the start of b and returns it with the
// number of bytes read. Truncated, overflowing and non-minimal encodings
// are CORRUPT (SPEC §7.1).
func ReadUvarint(b []byte) (uint64, int, error) {
	const op = "codec.ReadUvarint"
	var x uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		if i == binary.MaxVarintLen64-1 && c > 1 {
			return 0, 0, errs.New(errs.CodeCorrupt, op, "uvarint overflows 64 bits")
		}
		if c < 0x80 {
			if i > 0 && c == 0 {
				return 0, 0, errs.New(errs.CodeCorrupt, op, "uvarint is not in minimal form")
			}
			return x | uint64(c)<<shift, i + 1, nil
		}
		x |= uint64(c&0x7f) << shift
		shift += 7
	}
	return 0, 0, errs.New(errs.CodeCorrupt, op, "uvarint is truncated")
}
````

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./internal/codec`

Expected: `ok  github.com/TacFlow/tacdb/internal/codec`.

- [ ] **Step 5: Commit**

```bash
git add internal/codec
git commit -m "feat(codec): uvarint em forma mínima"
```


### Task 8: Codec G2 de valores

**Files:**
- Create: `internal/codec/value.go`
- Test: `internal/codec/value_test.go`

- [ ] **Step 1: Escrever o teste que falha**

Criar `internal/codec/value_test.go` com este conteúdo:

````go
package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/valuetest"
	"github.com/TacFlow/tacdb/value"
)

func defaultOpts() DecodeOptions { return DecodeOptions{Limits: value.DefaultLimits()} }

func mustEncode(t *testing.T, v value.Value) []byte {
	t.Helper()
	b, err := AppendValue(nil, v, value.DefaultLimits())
	if err != nil {
		t.Fatalf("AppendValue(%v): %v", v, err)
	}
	return b
}

// VAL-01 and VAL-03: every kind round-trips bit for bit.
func TestRoundTripPreservesBits(t *testing.T) {
	vals := []value.Value{
		value.Null(), value.Bool(false), value.Bool(true),
		value.Int(math.MaxInt64), value.Int(math.MinInt64), value.Int(1<<53 + 1),
		value.Float(math.Copysign(0, -1)), value.Float(math.Inf(-1)),
		value.Float(math.Float64frombits(0x7ff8000000000001)),
		value.String(""), value.String("a\x00ção"),
		value.Bytes(nil), value.Bytes([]byte{0, 0xff, 0}),
		value.Vector([]float32{float32(math.NaN()), float32(math.Copysign(0, -1)), 1}),
		value.List(), value.List(value.Null(), value.List(value.Int(1))),
		value.MapOf(map[string]value.Value{"": value.Null(), "k": value.MapOf(map[string]value.Value{"x": value.Bool(true)})}),
	}
	for _, v := range vals {
		enc := mustEncode(t, v)
		if int64(len(enc)) != EncodedSize(v) {
			t.Errorf("EncodedSize(%v) = %d, encoding has %d bytes", v, EncodedSize(v), len(enc))
		}
		got, err := DecodeValue(enc, defaultOpts())
		if err != nil {
			t.Fatalf("DecodeValue(%v): %v", v, err)
		}
		if !value.Identical(got, v) {
			t.Errorf("round trip of %v gave %v", v, got)
		}
	}
}

func TestRandomRoundTripIsCanonical(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 2000; i++ {
		v := valuetest.Value(r, 4)
		enc := mustEncode(t, v)
		got, err := DecodeValue(enc, defaultOpts())
		if err != nil {
			t.Fatalf("DecodeValue(%v): %v", v, err)
		}
		if !value.Identical(got, v) {
			t.Fatalf("round trip of %v gave %v", v, got)
		}
		if again := mustEncode(t, got); !bytes.Equal(again, enc) {
			t.Fatalf("re-encoding %v changed the bytes", v)
		}
	}
}

func TestAppendValueValidates(t *testing.T) {
	if _, err := AppendValue(nil, value.String("\xff"), value.DefaultLimits()); !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Errorf("invalid UTF-8: err = %v, want INVALID_ARGUMENT", err)
	}
	lim := value.DefaultLimits()
	lim.MaxValueBytes = 10
	if _, err := AppendValue(nil, value.String("0123456789"), lim); !errors.Is(err, errs.Sentinel(errs.CodeLimitExceeded)) {
		t.Errorf("15-byte encoding with limit 10: err = %v, want LIMIT_EXCEEDED", err)
	}
}

func u32(n uint32) []byte { return binary.LittleEndian.AppendUint32(nil, n) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestDecodeRejectsMalformed(t *testing.T) {
	corrupt := errs.Sentinel(errs.CodeCorrupt)
	cases := map[string][]byte{
		"empty":              nil,
		"unknown tag":        {0x09},
		"bool 2":             {0x01, 0x02},
		"truncated int":      {0x02, 1, 2, 3},
		"trailing byte":      {0x00, 0x00},
		"string past end":    cat([]byte{0x04}, u32(5), []byte("abc")),
		"invalid UTF-8":      cat([]byte{0x04}, u32(1), []byte{0xff}),
		"vector dimension 0": cat([]byte{0x06}, u32(0)),
		"vector too wide":    cat([]byte{0x06}, u32(65536)),
		"list count lies":    cat([]byte{0x07}, u32(3), []byte{0x00}),
		"map out of order": cat([]byte{0x08}, u32(2),
			u32(1), []byte("b"), []byte{0x00},
			u32(1), []byte("a"), []byte{0x00}),
		"map duplicate key": cat([]byte{0x08}, u32(2),
			u32(1), []byte("a"), []byte{0x00},
			u32(1), []byte("a"), []byte{0x00}),
		"map key bad UTF-8": cat([]byte{0x08}, u32(1), u32(1), []byte{0xc3}, []byte{0x00}),
	}
	for name, b := range cases {
		if _, err := DecodeValue(b, defaultOpts()); !errors.Is(err, corrupt) {
			t.Errorf("%s: err = %v, want CORRUPT", name, err)
		}
	}
}

func TestDecodeRejectsAboveLimits(t *testing.T) {
	limitExceeded := errs.Sentinel(errs.CodeLimitExceeded)
	deep := []byte{}
	for i := 0; i < 64; i++ {
		deep = append(deep, cat([]byte{0x07}, u32(1))...)
	}
	deep = append(deep, 0x00) // 65 levels
	bigKey := strings.Repeat("k", 64<<10+1)
	cases := map[string][]byte{
		"depth 65":           deep,
		"vector above 4096":  cat([]byte{0x06}, u32(4097), make([]byte, 4*4097)),
		"map key above 64 K": cat([]byte{0x08}, u32(1), u32(uint32(len(bigKey))), []byte(bigKey), []byte{0x00}),
	}
	for name, b := range cases {
		if _, err := DecodeValue(b, defaultOpts()); !errors.Is(err, limitExceeded) {
			t.Errorf("%s: err = %v, want LIMIT_EXCEEDED", name, err)
		}
	}
}

type countingBudget struct {
	debited, limit int64
}

func (b *countingBudget) Debit(n int64) error {
	b.debited += n
	if b.debited > b.limit {
		return errs.New(errs.CodeResourceLimit, "test.budget", "exhausted").With("limit", b.limit)
	}
	return nil
}

// VAL-05: 16 MiB of minimal elements is rejected before the element slice
// is allocated, both by MaxValueElements and by the budget.
func TestDecodeChargesBeforeAllocating(t *testing.T) {
	const n = 16<<20 - 5
	nulls := cat([]byte{0x07}, u32(n), make([]byte, n))

	if _, err := DecodeValue(nulls, defaultOpts()); !errors.Is(err, errs.Sentinel(errs.CodeLimitExceeded)) {
		t.Fatalf("16M nulls with default limits: err = %v, want LIMIT_EXCEEDED", err)
	}

	opts := defaultOpts()
	opts.Limits.MaxValueElements = math.MaxInt32
	budget := &countingBudget{limit: 1 << 20}
	opts.Budget = budget
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := DecodeValue(nulls, opts)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errs.Sentinel(errs.CodeResourceLimit)) {
		t.Fatalf("budget of 1 MiB: err = %v, want RESOURCE_LIMIT", err)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 1<<20 {
		t.Fatalf("decoder allocated %d bytes before failing", grown)
	}
}
````

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/codec`

Expected: FAIL de compilação: `undefined: DecodeOptions`.

- [ ] **Step 3: Escrever o codec**

Criar `internal/codec/value.go` com este conteúdo:

````go
package codec

import (
	"encoding/binary"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Budget is charged for decoding work before memory is allocated, so that
// a small encoding cannot expand into a huge decoded value (SPEC §5.3).
type Budget interface {
	Debit(bytes int64) error
}

// DecodeOptions controls DecodeValue.
type DecodeOptions struct {
	Limits value.Limits
	Budget Budget // optional
}

// elementCost is the minimum charge per decoded element (SPEC §5.3).
const elementCost = 64

// EncodedSize returns the size of the G2 encoding of v.
func EncodedSize(v value.Value) int64 {
	switch v.Kind() {
	case value.KindBool:
		return 2
	case value.KindInt, value.KindFloat:
		return 9
	case value.KindString, value.KindBytes:
		return 5 + int64(v.Len())
	case value.KindVector:
		return 5 + 4*int64(v.Len())
	case value.KindList:
		n := int64(5)
		for i := 0; i < v.Len(); i++ {
			n += EncodedSize(v.Index(i))
		}
		return n
	case value.KindMap:
		n := int64(5)
		for i := 0; i < v.Len(); i++ {
			k, item := v.EntryAt(i)
			n += 4 + int64(len(k)) + EncodedSize(item)
		}
		return n
	}
	return 1 // Null
}

// AppendValue validates v against lim and appends its G2 encoding to dst.
// Integers are little-endian; maps are written in key order, which makes
// the encoding canonical.
func AppendValue(dst []byte, v value.Value, lim value.Limits) ([]byte, error) {
	if err := value.Validate(v, lim); err != nil {
		return dst, err
	}
	size := EncodedSize(v)
	if size > int64(lim.MaxValueBytes) {
		return dst, errs.New(errs.CodeLimitExceeded, "codec.AppendValue", "encoded value too large").
			With("bytes", size).With("limit", int64(lim.MaxValueBytes))
	}
	dst = slices.Grow(dst, int(size))
	return appendValue(dst, v), nil
}

func appendValue(dst []byte, v value.Value) []byte {
	dst = append(dst, byte(v.Kind()))
	switch v.Kind() {
	case value.KindBool:
		if b, _ := v.AsBool(); b {
			return append(dst, 1)
		}
		return append(dst, 0)
	case value.KindInt:
		i, _ := v.AsInt()
		return binary.LittleEndian.AppendUint64(dst, uint64(i))
	case value.KindFloat:
		f, _ := v.AsFloat()
		return binary.LittleEndian.AppendUint64(dst, math.Float64bits(f))
	case value.KindString, value.KindBytes:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Len()))
		return v.AppendBytes(dst)
	case value.KindVector:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(v.VectorAt(i)))
		}
		return dst
	case value.KindList:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			dst = appendValue(dst, v.Index(i))
		}
		return dst
	case value.KindMap:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			k, item := v.EntryAt(i)
			dst = binary.LittleEndian.AppendUint32(dst, uint32(len(k)))
			dst = append(dst, k...)
			dst = appendValue(dst, item)
		}
		return dst
	}
	return dst // Null
}

// DecodeValue decodes exactly one G2 value occupying all of b.
//
// Every length is checked against the remaining bytes and every allocation
// is charged to opt.Budget before it happens. Malformed or non-canonical
// input is CORRUPT; well-formed input above opt.Limits is LIMIT_EXCEEDED.
func DecodeValue(b []byte, opt DecodeOptions) (value.Value, error) {
	if len(b) > opt.Limits.MaxValueBytes {
		return value.Value{}, errs.New(errs.CodeLimitExceeded, "codec.DecodeValue", "encoded value too large").
			With("bytes", int64(len(b))).With("limit", int64(opt.Limits.MaxValueBytes))
	}
	d := decoder{b: b, opt: opt}
	if err := d.charge(elementCost); err != nil {
		return value.Value{}, err
	}
	v, err := d.value(1)
	if err != nil {
		return value.Value{}, err
	}
	if d.off != len(b) {
		return value.Value{}, d.corrupt("%d trailing bytes", len(b)-d.off)
	}
	return v, nil
}

type decoder struct {
	b        []byte
	off      int
	opt      DecodeOptions
	elements int
}

const decodeOp = "codec.DecodeValue"

func (d *decoder) corrupt(format string, args ...any) error {
	return errs.Newf(errs.CodeCorrupt, decodeOp, "offset %d: "+format, append([]any{d.off}, args...)...)
}

func (d *decoder) charge(n int64) error {
	if d.opt.Budget == nil {
		return nil
	}
	return d.opt.Budget.Debit(n)
}

func (d *decoder) remaining() int { return len(d.b) - d.off }

func (d *decoder) u8() (byte, error) {
	if d.remaining() < 1 {
		return 0, d.corrupt("truncated")
	}
	c := d.b[d.off]
	d.off++
	return c, nil
}

func (d *decoder) u32() (uint32, error) {
	if d.remaining() < 4 {
		return 0, d.corrupt("truncated")
	}
	x := binary.LittleEndian.Uint32(d.b[d.off:])
	d.off += 4
	return x, nil
}

func (d *decoder) u64() (uint64, error) {
	if d.remaining() < 8 {
		return 0, d.corrupt("truncated")
	}
	x := binary.LittleEndian.Uint64(d.b[d.off:])
	d.off += 8
	return x, nil
}

// span reserves n payload bytes after charging them to the budget.
func (d *decoder) span(n uint32) ([]byte, error) {
	if int64(n) > int64(d.remaining()) {
		return nil, d.corrupt("length %d exceeds the %d remaining bytes", n, d.remaining())
	}
	if err := d.charge(int64(n)); err != nil {
		return nil, err
	}
	s := d.b[d.off : d.off+int(n)]
	d.off += int(n)
	return s, nil
}

// count registers n new elements and charges them before any allocation.
func (d *decoder) count(n uint32, minBytesEach int) error {
	if int64(n)*int64(minBytesEach) > int64(d.remaining()) {
		return d.corrupt("%d elements cannot fit in the %d remaining bytes", n, d.remaining())
	}
	d.elements += int(n)
	if d.elements > d.opt.Limits.MaxValueElements {
		return errs.New(errs.CodeLimitExceeded, decodeOp, "too many elements").
			With("elements", int64(d.elements)).With("limit", int64(d.opt.Limits.MaxValueElements))
	}
	return d.charge(elementCost * int64(n))
}

func (d *decoder) value(depth int) (value.Value, error) {
	if depth > d.opt.Limits.MaxDepth {
		return value.Value{}, errs.New(errs.CodeLimitExceeded, decodeOp, "value nested too deeply").
			With("depth", int64(depth)).With("limit", int64(d.opt.Limits.MaxDepth))
	}
	tag, err := d.u8()
	if err != nil {
		return value.Value{}, err
	}
	switch value.Kind(tag) {
	case value.KindNull:
		return value.Null(), nil
	case value.KindBool:
		c, err := d.u8()
		if err != nil {
			return value.Value{}, err
		}
		if c > 1 {
			return value.Value{}, d.corrupt("bool byte %d", c)
		}
		return value.Bool(c == 1), nil
	case value.KindInt:
		x, err := d.u64()
		return value.Int(int64(x)), err
	case value.KindFloat:
		x, err := d.u64()
		return value.Float(math.Float64frombits(x)), err
	case value.KindString, value.KindBytes:
		n, err := d.u32()
		if err != nil {
			return value.Value{}, err
		}
		s, err := d.span(n)
		if err != nil {
			return value.Value{}, err
		}
		if value.Kind(tag) == value.KindBytes {
			return value.Bytes(s), nil
		}
		if !utf8.Valid(s) {
			return value.Value{}, d.corrupt("string is not valid UTF-8")
		}
		return value.String(string(s)), nil
	case value.KindVector:
		return d.vector()
	case value.KindList:
		n, err := d.u32()
		if err != nil {
			return value.Value{}, err
		}
		if err := d.count(n, 1); err != nil {
			return value.Value{}, err
		}
		return value.MakeList(int(n), func(int) (value.Value, error) { return d.value(depth + 1) })
	case value.KindMap:
		return d.mapValue(depth)
	}
	d.off--
	return value.Value{}, d.corrupt("unknown tag 0x%02x", tag)
}

func (d *decoder) vector() (value.Value, error) {
	n, err := d.u32()
	if err != nil {
		return value.Value{}, err
	}
	if n == 0 || n > value.FormatMaxVectorDims {
		return value.Value{}, d.corrupt("vector dimension %d", n)
	}
	if int(n) > d.opt.Limits.MaxVectorDimensions {
		return value.Value{}, errs.New(errs.CodeLimitExceeded, decodeOp, "vector dimension above limit").
			With("dimensions", int64(n)).With("limit", int64(d.opt.Limits.MaxVectorDimensions))
	}
	raw, err := d.span(4 * n)
	if err != nil {
		return value.Value{}, err
	}
	return value.MakeVector(int(n), func(i int) float32 {
		return math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}), nil
}

func (d *decoder) mapValue(depth int) (value.Value, error) {
	n, err := d.u32()
	if err != nil {
		return value.Value{}, err
	}
	if err := d.count(n, 5); err != nil { // key length + tag
		return value.Value{}, err
	}
	var prev string
	return value.MakeMap(int(n), func(i int) (value.Entry, error) {
		klen, err := d.u32()
		if err != nil {
			return value.Entry{}, err
		}
		if int(klen) > d.opt.Limits.MaxMapKeyBytes {
			return value.Entry{}, errs.New(errs.CodeLimitExceeded, decodeOp, "map key too long").
				With("bytes", int64(klen)).With("limit", int64(d.opt.Limits.MaxMapKeyBytes))
		}
		raw, err := d.span(klen)
		if err != nil {
			return value.Entry{}, err
		}
		if !utf8.Valid(raw) {
			return value.Entry{}, d.corrupt("map key is not valid UTF-8")
		}
		key := string(raw)
		if i > 0 && key <= prev {
			return value.Entry{}, d.corrupt("map keys out of order or duplicated")
		}
		prev = key
		item, err := d.value(depth + 1)
		if err != nil {
			return value.Entry{}, err
		}
		return value.Entry{Key: key, Value: item}, nil
	})
}
````

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./internal/codec`

Expected: `ok  github.com/TacFlow/tacdb/internal/codec`. O teste `TestDecodeChargesBeforeAllocating` monta um buffer de 16 MiB, mas termina rápido: a decodificação falha antes de alocar.

- [ ] **Step 5: Commit**

```bash
git add internal/codec/value.go internal/codec/value_test.go
git commit -m "feat(codec): codec G2 de valores com orçamento antes de alocar"
```


### Task 9: Chaves compostas

**Files:**
- Create: `internal/codec/key.go`
- Test: `internal/codec/key_test.go`

- [ ] **Step 1: Escrever o teste que falha**

Criar `internal/codec/key_test.go` com este conteúdo:

````go
package codec

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func TestKeyStringRoundTrip(t *testing.T) {
	for _, s := range []string{"", "a", "\x00", "a\x00b", "\x00\x00", "\xff", "ação"} {
		key := AppendKeyString(nil, s)
		key = AppendKeyUint32(key, 7)
		r := NewKeyReader(key)
		got, err := r.ReadString()
		if err != nil || got != s {
			t.Fatalf("ReadString(%x) = %q, %v; want %q", key, got, err, s)
		}
		if n, err := r.ReadUint32(); err != nil || n != 7 || r.Len() != 0 {
			t.Fatalf("component after %q: %d, %v, %d left", s, n, err, r.Len())
		}
	}
}

func TestKeyIntegersAreBigEndian(t *testing.T) {
	key := AppendKeyUint64(AppendKeyUint16(AppendKeyUint8(nil, 1), 0x0203), 0x0405060708090a0b)
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	if !bytes.Equal(key, want) {
		t.Fatalf("key = %x, want %x", key, want)
	}
	r := NewKeyReader(key)
	a, _ := r.ReadUint8()
	b, _ := r.ReadUint16()
	c, err := r.ReadUint64()
	if a != 1 || b != 0x0203 || c != 0x0405060708090a0b || err != nil {
		t.Fatalf("read back %d %d %d %v", a, b, c, err)
	}
	if _, err := r.ReadUint8(); !errors.Is(err, errs.Sentinel(errs.CodeCorrupt)) {
		t.Fatalf("reading past the end: err = %v, want CORRUPT", err)
	}
}

// The encoded order of (string, uint64) keys must equal the logical order,
// including strings that contain 0x00 and strings that are prefixes of
// others.
func TestKeyOrderMatchesLogicalOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	alphabet := []byte{0x00, 0x01, 'a', 0xfe, 0xff}
	randomString := func() string {
		b := make([]byte, r.IntN(5))
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		return string(b)
	}
	for i := 0; i < 20000; i++ {
		s1, s2 := randomString(), randomString()
		n1, n2 := r.Uint64N(3), r.Uint64N(3)
		k1 := AppendKeyUint64(AppendKeyString(nil, s1), n1)
		k2 := AppendKeyUint64(AppendKeyString(nil, s2), n2)
		want := strings.Compare(s1, s2)
		if want == 0 {
			want = cmpU64(n1, n2)
		}
		if got := bytes.Compare(k1, k2); got != want {
			t.Fatalf("(%q,%d) vs (%q,%d): bytes order %d, logical order %d", s1, n1, s2, n2, got, want)
		}
		if strings.HasPrefix(s2, s1) && !bytes.HasPrefix(k2, AppendKeyStringPrefix(nil, s1)) {
			t.Fatalf("prefix encoding of %q is not a prefix of the key of %q", s1, s2)
		}
	}
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestReadStringRejects(t *testing.T) {
	cases := map[string][]byte{
		"no terminator":  []byte("abc"),
		"lone zero":      {'a', 0x00},
		"invalid escape": {0x00, 0x01, 0x00, 0x00},
	}
	for name, key := range cases {
		if _, err := NewKeyReader(key).ReadString(); !errors.Is(err, errs.Sentinel(errs.CodeCorrupt)) {
			t.Errorf("%s: err = %v, want CORRUPT", name, err)
		}
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{1, 2}, []byte{1, 3}},
		{[]byte{1, 0xff}, []byte{2}},
		{[]byte{0xff, 0xff}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		if got := PrefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("PrefixEnd(%x) = %x, want %x", c.in, got, c.want)
		}
	}
	in := []byte{1, 2}
	PrefixEnd(in)
	if in[1] != 2 {
		t.Error("PrefixEnd modified its input")
	}
}

func TestCheckKey(t *testing.T) {
	if err := CheckKey(make([]byte, MaxKeyLen)); err != nil {
		t.Fatalf("1024-byte key: %v", err)
	}
	if err := CheckKey(make([]byte, MaxKeyLen+1)); !errors.Is(err, errs.Sentinel(errs.CodeLimitExceeded)) {
		t.Fatalf("1025-byte key: err = %v, want LIMIT_EXCEEDED", err)
	}
}
````

- [ ] **Step 2: Rodar e ver falhar**

Run: `go test ./internal/codec`

Expected: FAIL de compilação: `undefined: AppendKeyString`.

- [ ] **Step 3: Escrever as chaves compostas**

Criar `internal/codec/key.go` com este conteúdo:

````go
package codec

import (
	"encoding/binary"

	"github.com/TacFlow/tacdb/internal/errs"
)

// MaxKeyLen is the maximum length of an encoded B+tree key (SPEC §7.3).
const MaxKeyLen = 1024

// Composite keys (SPEC §7.4) concatenate components whose byte order
// matches their logical order: unsigned integers are big-endian, and
// strings escape 0x00 as 0x00 0xFF and end with 0x00 0x00.

// AppendKeyUint8 appends v.
func AppendKeyUint8(dst []byte, v uint8) []byte { return append(dst, v) }

// AppendKeyUint16 appends v big-endian.
func AppendKeyUint16(dst []byte, v uint16) []byte { return binary.BigEndian.AppendUint16(dst, v) }

// AppendKeyUint32 appends v big-endian.
func AppendKeyUint32(dst []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(dst, v) }

// AppendKeyUint64 appends v big-endian.
func AppendKeyUint64(dst []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(dst, v) }

// AppendKeyString appends s escaped and terminated.
func AppendKeyString(dst []byte, s string) []byte {
	return append(AppendKeyStringPrefix(dst, s), 0x00, 0x00)
}

// AppendKeyStringPrefix appends s escaped but not terminated. The result is
// a prefix of the encoding of every string that starts with s, for prefix
// scans over a string component.
func AppendKeyStringPrefix(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x00 {
			dst = append(dst, 0x00, 0xFF)
		} else {
			dst = append(dst, s[i])
		}
	}
	return dst
}

// CheckKey returns LIMIT_EXCEEDED if key is longer than MaxKeyLen. Callers
// check before any mutation (SPEC §7.4).
func CheckKey(key []byte) error {
	if len(key) > MaxKeyLen {
		return errs.New(errs.CodeLimitExceeded, "codec.CheckKey", "key too long").
			With("bytes", int64(len(key))).With("limit", MaxKeyLen)
	}
	return nil
}

// PrefixEnd returns the smallest key greater than every key that starts
// with prefix, or nil if there is none (the prefix is empty or all 0xFF).
func PrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// KeyReader decodes the components of a composite key in order.
type KeyReader struct {
	b   []byte
	off int
}

// NewKeyReader returns a reader over key.
func NewKeyReader(key []byte) *KeyReader { return &KeyReader{b: key} }

// Len returns the number of unread bytes.
func (r *KeyReader) Len() int { return len(r.b) - r.off }

const keyOp = "codec.KeyReader"

func (r *KeyReader) fixed(n int) ([]byte, error) {
	if r.Len() < n {
		return nil, errs.Newf(errs.CodeCorrupt, keyOp, "offset %d: truncated key", r.off)
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s, nil
}

// ReadUint8 reads a uint8 component.
func (r *KeyReader) ReadUint8() (uint8, error) {
	s, err := r.fixed(1)
	if err != nil {
		return 0, err
	}
	return s[0], nil
}

// ReadUint16 reads a big-endian uint16 component.
func (r *KeyReader) ReadUint16() (uint16, error) {
	s, err := r.fixed(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(s), nil
}

// ReadUint32 reads a big-endian uint32 component.
func (r *KeyReader) ReadUint32() (uint32, error) {
	s, err := r.fixed(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(s), nil
}

// ReadUint64 reads a big-endian uint64 component.
func (r *KeyReader) ReadUint64() (uint64, error) {
	s, err := r.fixed(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(s), nil
}

// ReadString reads an escaped, terminated string component.
func (r *KeyReader) ReadString() (string, error) {
	var out []byte
	for {
		if r.Len() == 0 {
			return "", errs.Newf(errs.CodeCorrupt, keyOp, "offset %d: unterminated string", r.off)
		}
		c := r.b[r.off]
		if c != 0x00 {
			out = append(out, c)
			r.off++
			continue
		}
		if r.Len() < 2 {
			return "", errs.Newf(errs.CodeCorrupt, keyOp, "offset %d: unterminated string", r.off)
		}
		switch r.b[r.off+1] {
		case 0x00:
			r.off += 2
			return string(out), nil
		case 0xFF:
			out = append(out, 0x00)
			r.off += 2
		default:
			return "", errs.Newf(errs.CodeCorrupt, keyOp, "offset %d: invalid escape 0x00 0x%02x", r.off, r.b[r.off+1])
		}
	}
}
````

- [ ] **Step 4: Rodar e ver passar**

Run: `go test ./internal/codec`

Expected: `ok  github.com/TacFlow/tacdb/internal/codec`.

- [ ] **Step 5: Commit**

```bash
git add internal/codec/key.go internal/codec/key_test.go
git commit -m "feat(codec): chaves compostas com escape de 0x00 e PrefixEnd"
```


### Task 10: Goldens e alvos de fuzz

**Files:**
- Create: `internal/codec/testdata/golden/values.golden`
- Test: `internal/codec/golden_test.go`
- Test: `internal/codec/fuzz_test.go`

- [ ] **Step 1: Escrever o teste de golden**

Criar `internal/codec/golden_test.go` com este conteúdo:

````go
package codec

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/value"
)

var update = flag.Bool("update", false, "rewrite golden files")

const goldenPath = "testdata/golden/values.golden"

type goldenValue struct {
	name string
	v    value.Value
}

// goldenValues fixes the G2 bytes of representative values (FORMAT.md).
func goldenValues() []goldenValue {
	return []goldenValue{
		{"null", value.Null()},
		{"bool-false", value.Bool(false)},
		{"bool-true", value.Bool(true)},
		{"int-zero", value.Int(0)},
		{"int-minus-one", value.Int(-1)},
		{"int-2^53+1", value.Int(1<<53 + 1)},
		{"int-min", value.Int(math.MinInt64)},
		{"float-one", value.Float(1)},
		{"float-minus-zero", value.Float(math.Copysign(0, -1))},
		{"float-nan-payload", value.Float(math.Float64frombits(0x7ff8000000000001))},
		{"float-plus-inf", value.Float(math.Inf(1))},
		{"string-empty", value.String("")},
		{"string-acao", value.String("ação")},
		{"string-with-nul", value.String("a\x00b")},
		{"bytes", value.Bytes([]byte{0x00, 0x01, 0xff})},
		{"vector", value.Vector([]float32{1, 2})},
		{"list", value.List(value.Int(1), value.Null())},
		{"map", value.MapOf(map[string]value.Value{"origem": value.String("manual"), "a": value.Bool(true)})},
		{"nested", value.List(value.MapOf(map[string]value.Value{"k": value.List()}))},
	}
}

func TestGoldenEncodings(t *testing.T) {
	var buf bytes.Buffer
	for _, g := range goldenValues() {
		enc, err := AppendValue(nil, g.v, value.DefaultLimits())
		if err != nil {
			t.Fatalf("%s: %v", g.name, err)
		}
		fmt.Fprintf(&buf, "%s %s\n", g.name, hex.EncodeToString(enc))
	}
	if *update {
		if err := os.WriteFile(goldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/codec -run TestGoldenEncodings -update` once and review the file)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("encodings differ from %s:\n got:\n%s\nwant:\n%s", goldenPath, buf.String(), want)
	}
	// Every golden line must also decode back to its value.
	sc := bufio.NewScanner(bytes.NewReader(want))
	for i := 0; sc.Scan(); i++ {
		name, hexed, _ := strings.Cut(sc.Text(), " ")
		raw, err := hex.DecodeString(hexed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := DecodeValue(raw, DecodeOptions{Limits: value.DefaultLimits()})
		if err != nil || !value.Identical(got, goldenValues()[i].v) {
			t.Fatalf("%s: decoded %v, %v", name, got, err)
		}
	}
}
````

- [ ] **Step 2: Escrever os alvos de fuzz**

Criar `internal/codec/fuzz_test.go` com este conteúdo:

````go
package codec

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/valuetest"
	"github.com/TacFlow/tacdb/value"
)

// Target names must not be substrings of each other: CI runs the literal
// gate `go test ./internal/codec -fuzz=FuzzValue` (SPEC §25.4), and -fuzz
// must match exactly one target.

// FuzzValue decodes arbitrary bytes. It must never panic, and a successful
// decode must re-encode to the same bytes (canonical form).
func FuzzValue(f *testing.F) {
	for _, g := range goldenValues() {
		enc, err := AppendValue(nil, g.v, value.DefaultLimits())
		if err != nil {
			f.Fatal(err)
		}
		f.Add(enc)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := DecodeValue(data, DecodeOptions{Limits: value.DefaultLimits()})
		if err != nil {
			return
		}
		enc, err := AppendValue(nil, v, value.DefaultLimits())
		if err != nil {
			t.Fatalf("decoded value does not re-encode: %v", err)
		}
		if !bytes.Equal(enc, data) {
			t.Fatalf("non-canonical input accepted:\n in  %x\n out %x", data, enc)
		}
	})
}

// FuzzRoundTrip encodes generated values and decodes them back.
func FuzzRoundTrip(f *testing.F) {
	f.Add(uint64(1), uint8(3))
	f.Fuzz(func(t *testing.T, seed uint64, depth uint8) {
		v := valuetest.Value(rand.New(rand.NewPCG(seed, 0)), 1+int(depth%6))
		enc, err := AppendValue(nil, v, value.DefaultLimits())
		if err != nil {
			t.Fatalf("AppendValue(%v): %v", v, err)
		}
		got, err := DecodeValue(enc, DecodeOptions{Limits: value.DefaultLimits()})
		if err != nil || !value.Identical(got, v) {
			t.Fatalf("round trip of %v gave %v, %v", v, got, err)
		}
	})
}

// FuzzUvarint checks that accepted uvarints are minimal.
func FuzzUvarint(f *testing.F) {
	f.Add([]byte{0x80, 0x01})
	f.Add([]byte{0x80, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		x, n, err := ReadUvarint(data)
		if err != nil {
			return
		}
		if !bytes.Equal(AppendUvarint(nil, x), data[:n]) || UvarintLen(x) != n {
			t.Fatalf("accepted non-minimal %x for %d", data[:n], x)
		}
	})
}

// FuzzKeyReader reads arbitrary bytes as key components. It must never
// panic, and a string it accepts must re-encode to the bytes it consumed.
func FuzzKeyReader(f *testing.F) {
	f.Add([]byte{'a', 0x00, 0xff, 0x00, 0x00, 1, 2})
	f.Add([]byte{0x00, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewKeyReader(data)
		s, err := r.ReadString()
		if err == nil {
			consumed := data[:len(data)-r.Len()]
			if enc := AppendKeyString(nil, s); !bytes.Equal(enc, consumed) {
				t.Fatalf("ReadString accepted %x but re-encodes to %x", consumed, enc)
			}
		}
		r = NewKeyReader(data)
		for r.Len() > 0 {
			if _, err := r.ReadUint64(); err != nil {
				break
			}
		}
	})
}

// FuzzKeyEncoding checks order preservation and round trips of string
// components.
func FuzzKeyEncoding(f *testing.F) {
	f.Add("a", "a\x00")
	f.Add("\x00\xff", "\x00")
	f.Fuzz(func(t *testing.T, a, b string) {
		ka, kb := AppendKeyString(nil, a), AppendKeyString(nil, b)
		if got, want := bytes.Compare(ka, kb), strings.Compare(a, b); got != want {
			t.Fatalf("order of %q vs %q: bytes %d, strings %d", a, b, got, want)
		}
		got, err := NewKeyReader(ka).ReadString()
		if err != nil || got != a {
			t.Fatalf("round trip of %q gave %q, %v", a, got, err)
		}
	})
}
````

- [ ] **Step 3: Rodar e ver falhar**

Run: `go test ./internal/codec -run TestGoldenEncodings`

Expected: FAIL: `open testdata/golden/values.golden: no such file or directory`.

- [ ] **Step 4: Criar o arquivo golden**

Criar `internal/codec/testdata/golden/values.golden` com este conteúdo:

````text
null 00
bool-false 0100
bool-true 0101
int-zero 020000000000000000
int-minus-one 02ffffffffffffffff
int-2^53+1 020100000000002000
int-min 020000000000000080
float-one 03000000000000f03f
float-minus-zero 030000000000000080
float-nan-payload 03010000000000f87f
float-plus-inf 03000000000000f07f
string-empty 0400000000
string-acao 040600000061c3a7c3a36f
string-with-nul 0403000000610062
bytes 05030000000001ff
vector 06020000000000803f00000040
list 070200000002010000000000000000
map 080200000001000000610101060000006f726967656d04060000006d616e75616c
nested 07010000000801000000010000006b0700000000
````

Esses bytes foram conferidos contra a SPEC §5.3 (por exemplo, `Int(-1)` é `02` seguido de oito `ff`, e o mapa sai em ordem de chave). Não regenere com `-update` sem revisar a diferença.

- [ ] **Step 5: Rodar e ver passar**

Run: `go test ./internal/codec`

Expected: `ok  github.com/TacFlow/tacdb/internal/codec`.

- [ ] **Step 6: Rodar cada alvo de fuzz por 10 s**

Run: `for f in FuzzValue FuzzRoundTrip FuzzUvarint FuzzKeyEncoding FuzzKeyReader; do go test ./internal/codec -run '^$' -fuzz="^$f\$" -fuzztime=10s || break; done`

Expected: cinco `PASS`. Se um alvo falhar, o Go grava o input em `internal/codec/testdata/fuzz/<alvo>/`; corrija o código, mantenha o arquivo como regressão e rode de novo.

- [ ] **Step 7: Rodar o gate literal da SPEC §25.4**

Run: `go test ./internal/codec -fuzz=FuzzValue -fuzztime=10s`

Expected: `PASS` (o padrão `FuzzValue` precisa casar com um único alvo; por isso o outro se chama `FuzzRoundTrip`).

- [ ] **Step 8: Commit**

```bash
git add internal/codec
git commit -m "test(codec): goldens de valores e alvos de fuzz"
```


### Task 11: Documentação mínima do M0

**Files:**
- Create: `NOTICE`
- Create: `FORMAT.md`
- Create: `COMPATIBILITY.md`
- Create: `AUDIT-REGRESSIONS.md`

- [ ] **Step 1: Criar o NOTICE**

Criar `NOTICE` com este conteúdo:

````text
tacdb
Copyright (c) 2026 Tacflow

O tacdb é distribuído sob a licença MIT (arquivo LICENSE).

O tacdb reimplementa em Go as capacidades de um motor de referência
distribuído sob a licença MIT. As partes derivadas preservam o aviso de
copyright e a licença desse software, reproduzidos abaixo como a licença
exige (SPEC.md §2.1).

--------------------------------------------------------------------------

MIT License

Copyright (c) 2025 Jeff Hajewski

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
````

O bloco da licença MIT foi conferido byte a byte contra o arquivo de licença do motor de referência quando este plano foi preparado; não reformate as linhas. A licença BSD-3-Clause do `golang.org/x/sys` entra no M1b, junto com a dependência.

- [ ] **Step 2: Criar o FORMAT.md**

Criar `FORMAT.md` com este conteúdo:

````markdown
# FORMAT — formato G2 do tacdb

Este documento descreve os encodings do formato G2 **como estão implementados**, com exemplos que os testes verificam byte a byte. A fonte normativa é a `SPEC.md` (§5.3, §7 e §9); quando os dois divergirem, a SPEC prevalece e a divergência é um bug.

O documento cresce a cada marco. Nesta versão ele cobre o codec de valores, os uvarints e as chaves compostas (M1a). As seções de superbloco, páginas, extents e bitmaps entram com o pacote `internal/page` (M1c), e as de WAL com o M2.

## 1. Convenções

| Item | Regra |
|---|---|
| Inteiros de payload | Little-endian, largura fixa. |
| Inteiros em chaves de B+tree | Big-endian, para que a ordem dos bytes seja a ordem numérica. |
| Inteiros de comprimento variável | Uvarint (LEB128) em forma mínima (§3). |
| Strings | UTF-8 válido, sem normalização. |
| Forma canônica | Cada valor tem exatamente uma codificação; o decodificador recusa qualquer outra. |

## 2. Codec de valores

Implementação: `internal/codec/value.go`. Exemplos verificados: `internal/codec/testdata/golden/values.golden`.

Todo valor começa com uma tag de 1 byte:

| Tag | Tipo | Payload depois da tag |
|---|---|---|
| `0x00` | Null | Nenhum. |
| `0x01` | Bool | 1 byte: `0x00` ou `0x01`. |
| `0x02` | Int | int64, 8 bytes. |
| `0x03` | Float | Os 8 bytes de `math.Float64bits`; NaN com payload, infinitos e `-0` são preservados. |
| `0x04` | String | Comprimento u32 e os bytes UTF-8. |
| `0x05` | Bytes | Comprimento u32 e os bytes. |
| `0x06` | Vector | Dimensão u32 (1 a 65535) e os bits de cada componente float32 (4 bytes). |
| `0x07` | List | Quantidade u32 e os valores, recursivamente. |
| `0x08` | Map | Quantidade u32 e os pares `comprimento da chave u32 + chave UTF-8 + valor`, em ordem crescente de bytes da chave, sem repetição. |

Exemplos (hex):

| Valor | Codificação |
|---|---|
| `Int(-1)` | `02 ffffffffffffffff` |
| `Int(2^53+1)` | `02 0100000000002000` |
| `Float(1)` | `03 000000000000f03f` |
| `Float(-0)` | `03 0000000000000080` |
| `String("ação")` | `04 06000000 61c3a7c3a36f` |
| `Vector([1, 2])` | `06 02000000 0000803f 00000040` |
| `Map{"a": true, "origem": "manual"}` | `08 02000000 01000000 61 0101 06000000 6f726967656d 04 06000000 6d616e75616c` |

### 2.1 Decodificação

O decodificador recebe limites (`value.Limits`) e um orçamento opcional (`codec.Budget`).

| Situação | Resultado |
|---|---|
| Tag desconhecida, Bool fora de 0/1, UTF-8 inválido, dimensão de Vector 0 ou acima de 65535 | `CORRUPT` |
| Chaves de Map fora de ordem ou repetidas (forma não canônica) | `CORRUPT` |
| Comprimento ou quantidade maior que os bytes restantes | `CORRUPT`, antes de alocar |
| Bytes sobrando depois do valor | `CORRUPT` |
| Profundidade acima de `MaxDepth` (default 64; um escalar tem profundidade 1) | `LIMIT_EXCEEDED` |
| Elementos de List e Map, somados em todos os níveis, acima de `MaxValueElements` (default 1 000 000) | `LIMIT_EXCEEDED` |
| Chave de Map acima de `MaxMapKeyBytes` (default 64 KiB) | `LIMIT_EXCEEDED` |
| Dimensão de Vector acima de `MaxVectorDimensions` (default 4096, máximo 65535) | `LIMIT_EXCEEDED` |
| Codificação acima de `MaxValueBytes` (16 MiB) | `LIMIT_EXCEEDED` |

O orçamento é debitado **antes** de cada alocação: 64 bytes por elemento, mais os bytes variáveis de strings, bytes e vetores. Um erro do orçamento interrompe a decodificação e é devolvido como está.

## 3. Uvarint

Implementação: `internal/codec/uvarint.go`. É o LEB128 de `encoding/binary`, com uma regra extra: a forma precisa ser mínima. Um uvarint de mais de um byte cujo último byte é `0x00` não é mínimo e é `CORRUPT`, assim como um truncado ou maior que 64 bits.

| Valor | Bytes |
|---|---|
| 0 | `00` |
| 127 | `7f` |
| 128 | `80 01` |
| 1024 | `80 08` |
| 2^24 (16 MiB) | `80 80 80 08` |

## 4. Chaves compostas

Implementação: `internal/codec/key.go`. Uma chave de B+tree concatena componentes cuja ordem de bytes é a ordem lógica:

- Inteiros sem sinal: big-endian, largura fixa (u8, u16, u32, u64).
- Strings e bytes: cada `0x00` vira `0x00 0xFF`, e o componente termina com `0x00 0x00`. Assim `"a" < "a\x00"` também nos bytes (`61 00 00` antes de `61 00 FF 00 00`), e o terminador separa o componente do seguinte.
- Prefixo de string: a forma escapada **sem** terminador é prefixo da chave de toda string que começa com ela, e serve para varreduras por prefixo.
- Fim de faixa: `PrefixEnd(p)` devolve a menor chave maior que todas as chaves que começam com `p` (incrementa o último byte diferente de `0xFF`); devolve vazio quando não existe tal chave.
- Tamanho máximo de uma chave codificada: 1024 bytes (`CheckKey`), verificado antes de qualquer mutação.
````

- [ ] **Step 3: Criar o COMPATIBILITY.md**

Criar `COMPATIBILITY.md` com este conteúdo:

````markdown
# Matriz de compatibilidade

Política (SPEC §25.1 e §28): o tacdb busca paridade funcional e semântica com o motor de referência (R0) nas capacidades inventariadas. Toda diferença tem ID, motivo e teste nesta matriz; nenhuma divergência some sem registro.

## 1. Áreas

| Área | Política (SPEC §28) | Status |
|---|---|---|
| Modelo de propriedades/grafo | Paridade, incluindo ausência/Null, listas/maps e IDs de arestas. | Valores e codec prontos (M1a); grafo pendente (M3). |
| Dialeto Cypher da revisão | Paridade nos recursos inventariados. | Pendente (M4). |
| FTS/BM25/fuzzy | Paridade funcional; score determinístico entre plataformas. | Pendente (M5). |
| HNSW | Mesmas capacidades e métricas; topologia não é identidade binária. | Pendente (M8). |
| Transações | Um escritor em mutação; leitores paralelos; group commit adaptativo. | Pendente (M2). |
| Manutenção física | Checkpoint passivo, WAL segmentado, alocador por bitmap, scrub, compact. | Pendente (M1c a M2). |
| Cluster/multiprocesso | Obrigatórios em 1.0. | Pendente (M10 a M12). |
| Streams | Sequências, offsets, trim e atomicidade preservados. | Pendente (M6). |
| Segurança | Modelo da §20, sem equivalente em R0. | Pendente (M1b, M10). |
| Arquivo principal/WAL de R0 | Incompatíveis fisicamente; migração LTX1. | Por decisão (ADR-07). |
| API Go original | Adaptador de conformidade; API principal nova. | Pendente. |
| C ABI / Python / Java | Fora da distribuição inicial. | Por decisão. |
| TypeScript | SDK novo por bridge Go. | Pendente (M10, M13). |
| Melhorias Neo4j | Implementação Go própria dos contratos de §33, em clean room; sem compatibilidade binária, Bolt ou extensão Enterprise. | Pendente (M4 em diante). |
| Memória/serialize | VFS comum; Serialize produz snapshot G2 consistente sem quiescência. | MemoryVFS no M1b; Serialize pendente (M9). |
| Embedding helpers/serviços | Excluídos do runtime; vetores externos são aceitos opcionalmente. | Por decisão (ADR-03). |
| TAC | Linguagem 0.6 e IR 1.3 para o banco; novas construções não são aliases de skill. | Pendente (M7). |
| Construtos LAYA/SLM e memória antiga implícita | Ausentes do perfil; erro ou migração explícita de fonte. | Pendente (M7). |
| Performance | Sem promessa de melhoria automática; comparar e registrar; amplificação de escrita tem gate. | Pendente (benchmarks a partir do M2). |
| Limites G2 | IDs e sequências até MaxInt64; limites fixos exigem nova `format_revision`. | Limites de valor prontos (M1a). |

## 2. Decisões próprias do tacdb

| ID | Decisão | Motivo | Teste |
|---|---|---|---|
| D-001 | Ordem total de valores: Bool < número < String < Bytes < Vector < List < Map < Null. Int e Float comparam pelo valor numérico exato, sem converter int64 para float64; NaN fica depois de todos os números e empata com qualquer NaN; `-0` empata com `+0`. `Compare == 0` não implica bits iguais (`Int(1)` e `Float(1)` empatam). | A SPEC §5.3 fixa as famílias e pede casos próprios onde R0 não fixa o comportamento. | `value/compare_test.go` |
| D-002 | Codec canônico: o decodificador recusa chaves de Map fora de ordem ou repetidas, e uvarints não mínimos, como `CORRUPT`. | Cada valor tem uma única forma (SPEC 1.3 §5.3 e §7.1); goldens e assinaturas dependem disso. | `internal/codec/value_test.go`, `internal/codec/uvarint_test.go`, `FuzzValue` |

## 3. Divergências de R0 registradas

Nenhuma até aqui.
````

- [ ] **Step 4: Criar o AUDIT-REGRESSIONS.md**

Criar `AUDIT-REGRESSIONS.md` com este conteúdo:

````markdown
# Rastreabilidade de AUD, REV e R13

Cada item das tabelas §32.1, §32.4 e §32.5 da SPEC, com o marco em que precisa estar pronto e a evidência que o fecha. Um item só passa a **concluído** com teste ou medição citados; evidência parcial fica registrada como **parcial**.

| ID | Problema (resumo da SPEC) | Marco | Status | Evidência |
|---|---|---|---|---|
| AUD-01 | 200 leituras causaram 200 fsyncs; commit/abort sem gate read-only | onda 1 | pendente | — |
| AUD-02 | Planner k=100; LIMIT 500 devolveu 54–63 linhas | onda 1 | pendente | — |
| AUD-03 | Label de 5%, LIMIT 10: média de 3 linhas, às vezes 1 | onda 1/3 | pendente | — |
| AUD-04 | API C ignora key; segundo vetor sobrescreve primeiro | onda 1/3 | pendente | — |
| AUD-05 | Tx 1025 retorna falta de memória em vez de limite 1024 | onda 1 | pendente | — |
| AUD-06 | “LockTimeout” imediato, relatado em 26 µs | onda 1/2 | pendente | — |
| AUD-07 | Promise/async no binding TS bloqueia event loop por chamada nativa síncrona | onda 2 | pendente | — |
| AUD-08 | Design promete zero WAL em leitura; docs dizem rebuild HNSW, reopen de 20k vetores em 0,4 s | onda 1/3 | pendente | — |
| AUD-09 | 4× nós tornou commit 7–8× mais lento; leitura esperou 11,5 s durante commit de 12k nós | onda 2 | pendente | — |
| AUD-10 | Cypher vetorial percorre MATCH: 91/315 ms para 5k/20k vs 1–1,5 ms direto | onda 1/2 | pendente | — |
| AUD-11 | `<=>` dentro de AND perde índice; 1,2 s com 20k | onda 1 | pendente | — |
| AUD-12 | Recall@10 relatado de 54% com 20k; ef não ajustável no Cypher | onda 1/3 | pendente | — |
| AUD-13 | Insert vetorial caiu de 180/s para 127/s entre 5k e 20k | onda 2/3 | pendente | — |
| AUD-14 | >~2500 termos únicos leva update/delete a scan global; aviso só stderr | onda 1/3 | pendente | — |
| REV-01 | Checkpoint quiescente nunca roda sob leitura contínua; o WAL chega ao teto e as escritas falham; réplicas d… | M2 | pendente | — |
| REV-02 | Checkpoint bloqueava novos Begins durante cópia e syncs | M2 | pendente | — |
| REV-03 | Backup e Serialize bloqueavam o banco durante toda a cópia | M9 | pendente | — |
| REV-04 | Snapshot Raft pausava o apply; no líder, congelava os recibos | M11 | pendente | — |
| REV-05 | Snapshot esquecido prendia WAL e memória indefinidamente | M2 | pendente | — |
| REV-06 | Páginas quentes eram reemitidas por cada intent do grupo | M2 | pendente | — |
| REV-07 | Janela fixa de 2 ms penalizava o commit isolado | M2 | pendente | — |
| REV-08 | Um Sync dedicado a cada 1024 IDs | M2 | pendente | — |
| REV-09 | SHA-256 sobre todas as imagens no coordenador serial | M2 | pendente | — |
| REV-10 | FTS tocava centenas de páginas por documento | M5 | pendente | — |
| REV-11 | Fuzzy e prefixo sem limite de expansão | M5 | pendente | — |
| REV-12 | Ranking com LIMIT pontuava todos os matches | M5 | pendente | — |
| REV-13 | Vetores em overflow e seek por distância | M8 | pendente | — |
| REV-14 | Sem orçamento global de memória | M1 | pendente | — |
| REV-15 | Escritor único preso por operações longas | M2/M10 | pendente | — |
| REV-16 | DDL não indexava base grande online | M3 | pendente | — |
| REV-17 | Consumidor gravava uma WriteTx por evento ignorado | M7 | pendente | — |
| REV-18 | Metadados e propriedades em regiões distantes da árvore | M3 | pendente | — |
| REV-19 | Fanout baixo com chaves longas; slots largos | M1 | pendente | — |
| REV-20 | Overflow encadeado sem readahead; valores grandes poluíam o cache | M1 | pendente | — |
| REV-21 | Freelist LIFO espalhava páginas e anulava o compact | M1 | pendente | — |
| REV-22 | Strings longas perdiam faixa e prefixo no índice | M3 | pendente | — |
| REV-23 | MERGE sem índice custava O(N) por chamada | M3/M7 | pendente | — |
| REV-24 | Cache de planos poluído por literais | M4 | pendente | — |
| REV-25 | Savepoint por statement podia copiar o overlay | M2 | pendente | — |
| REV-26 | Contenção de RWMutex, registro de snapshots e LRU globais | M2 | pendente | — |
| REV-27 | Catálogo misturava declarações e contadores quentes; schemaEpoch ambíguo | M3 | pendente | — |
| REV-28 | Recibo de cluster exigia dois fsyncs em série | M11 | pendente | — |
| REV-29 | Bundles de até 65 MiB atravessavam o Raft | M11 | pendente | — |
| REV-30 | Uma rodada de ReadIndex por leitura | M12 | pendente | — |
| REV-31 | Deduplicação retinha resultados inteiros replicados | M12 | pendente | — |
| REV-32 | Protocolo JSON tipado pesado para vetores e resultados grandes | M10 | pendente | — |
| REV-33 | Recovery com verificação completa do banco | M2 | pendente | — |
| REV-34 | Resultados float podiam divergir entre plataformas | M4/M8 | pendente | — |
| REV-35 | Autenticação de clientes indefinida | M10 | pendente | — |
| REV-36 | Autorização sem modelo definido | M10 | pendente | — |
| REV-37 | Isolamento de tenants dependia de WHERE | M10/M12 | pendente | — |
| REV-38 | Changefeed, índices e logs expunham dados sensíveis | M6/M10 | pendente | — |
| REV-39 | Injeção armazenada e confused deputy por eventos | M7 | pendente | — |
| REV-40 | Identificadores dinâmicos montados por concatenação | M4 | pendente | — |
| REV-41 | DoS por consultas sem prazo, símbolos, decode e sessões | M10 | pendente | — |
| REV-42 | Permissões, symlinks e path traversal não especificados | M1 | pendente | — |
| REV-43 | Dados excluídos persistiam em changefeed, WAL, índices e backups | M9 | pendente | — |
| REV-44 | Hash sem chave tratado como garantia; arquivos forjados com CRC válido | M9 | pendente | — |
| REV-45 | Vazamento por erros, EXPLAIN, tokens e argv | M10 | pendente | — |
| REV-46 | Cadeia de suprimentos e bridge sem endurecimento | M13 | pendente | — |
| REV-47 | Fuzzing não cobria protocolos, manifestos e catálogo | M10–M13 | pendente | — |
| REV-48 | Código GPL citado como referência de implementação | M0 | pendente | — |
| R13-01 | Split em duas páginas impossível com células de cerca de 2 KiB (valor inline até 1024 bytes com chave de 10… | M1 | pendente | — |
| R13-02 | Página-descritor comporta 337 extents, sem garantia de que o valor caiba nela | M1 | pendente | — |
| R13-03 | `os.Root` segue symlinks dentro da raiz, contrariando “sem seguir symlinks” | M1 | pendente | — |
| R13-04 | Hard link do arquivo principal escapava do lock por nome | M1 | pendente | — |
| R13-05 | PageID 1, bitmaps e PageIDs ≥ pageCount não eram declarados inválidos como ponteiro de dados | M1 | pendente | — |
| R13-06 | Changefeed ligado por default, sem retenção automática, crescia sem limite | M6 | pendente | — |
| R13-07 | Payloads de changefeed importados misturavam o formato de R0 com o G2 no mesmo stream | M9 | pendente | — |
| R13-08 | `result.<nó>` invertia a convenção TAC `<nó>.result`; linhas só posicionais | M7 | pendente | — |
| R13-09 | WAL órfão de criação interrompida impedia `Open(Create)` | M2 | pendente | — |
| R13-10 | Go 1.27.2 já substituído por release de correções | M1 | parcial | M1a: `go.mod` com `toolchain go1.27.3`; vira concluído com o primeiro CI verde |
| R13-11 | Nomes inconsistentes: métrica de varredura de dicionário com dois nomes; `compatibility.md` | M0 | concluído | SPEC.md 1.3 (§2 e §13.3) |
| R13-12 | `created_unix_ns` impedia goldens binários byte a byte | M2 | pendente | — |
| R13-13 | Nome do motor de referência em código e documentação | M0 | parcial | SPEC.md 1.3 §2.1 e job `names` do CI; falta a variável `FORBIDDEN_NAMES_REGEX` no GitHub |
| R13-14 | Snowball pt/en “em Go puro” sem fonte definida | M5 | pendente | — |
| R13-15 | Ambiguidades de formato: comprimento usado na regra inline, `key_len` da célula interna, quando ligar PREFI… | M1 | pendente | — |
| R13-16 | Split, fusão e redistribuição medidos pelo prefixo da página original subestimavam o tamanho quando a chave… | M1 | pendente | — |
| R13-17 | `Error` sem campo para os detalhes numéricos que §6.1 exige | M1 | concluído | M1a: `internal/errs` (`Error.Details`) e testes de erro |
| R13-18 | §20.9 não dizia a quais objetos valem 0600/0700 e a checagem de dono; o diretório do chamador ficava ambíguo | M1 | pendente | — |
| R13-19 | A forma canônica com prefixo podia ser maior que a página sem prefixo, e decisões de caber tomadas pelo lei… | M1 | pendente | — |
| R13-20 | Cópia de superbloco de revisão futura podia ser classificada como danificada e reabrir em silêncio a cópia… | M1 | pendente | — |
| R13-21 | Permissões no Windows sem checagem de dono, ACLs estendidas do macOS ignoradas, junctions do Windows fora d… | M1 | pendente | — |
| R13-22 | Cursores de escrita sobreviviam à restauração de savepoint, e um contador restaurado com o savepoint permit… | M1/M2 | pendente | — |
````

As linhas reproduzem as tabelas §32.1, §32.4 e §32.5 da SPEC. Ao concluir um item em planos futuros, troque o status e cite o teste.

- [ ] **Step 5: Conferir os exemplos do FORMAT.md contra o golden**

Run: `grep -E '^(int-minus-one|int-2\^53\+1|float-one|float-minus-zero|string-acao|vector|map) ' internal/codec/testdata/golden/values.golden`

Expected: as mesmas sequências hex das tabelas da seção 2 do FORMAT.md, sem os espaços.

- [ ] **Step 6: Commit**

```bash
git add NOTICE FORMAT.md COMPATIBILITY.md AUDIT-REGRESSIONS.md
git commit -m "docs: NOTICE, FORMAT, COMPATIBILITY e AUDIT-REGRESSIONS iniciais"
```


### Task 12: CI

**Files:**
- Create: `.github/workflows/ci.yml`
- Create: `scripts/check-nocgo.sh`

- [ ] **Step 1: Criar o script de cgo**

Criar `scripts/check-nocgo.sh` com este conteúdo:

````bash
#!/usr/bin/env bash
# Falha se algum pacote não padrão do build de produção usar cgo (SPEC §25.4).
set -euo pipefail
pkgs=$(CGO_ENABLED=1 go list -deps -f '{{if and (not .Standard) .CgoFiles}}{{.ImportPath}}{{end}}' ./...)
if [ -n "$pkgs" ]; then
  echo "pacotes com arquivos cgo:" >&2
  echo "$pkgs" >&2
  exit 1
fi
echo "nenhum pacote não padrão usa cgo"
````

Depois: `chmod +x scripts/check-nocgo.sh`.

- [ ] **Step 2: Criar o workflow**

Criar `.github/workflows/ci.yml` com este conteúdo:

````yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  test:
    name: test (${{ matrix.os }})
    runs-on: ${{ matrix.os }}
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, macos-latest, windows-latest]
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - name: gofmt
        shell: bash
        run: |
          unformatted=$(gofmt -l .)
          if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
      - name: vet
        run: go vet ./...
      - name: test (CGO_ENABLED=0)
        env:
          CGO_ENABLED: "0"
        run: go test ./...
      - name: build (CGO_ENABLED=0)
        env:
          CGO_ENABLED: "0"
        run: go build ./...

  race:
    name: race (${{ matrix.os }})
    runs-on: ${{ matrix.os }}
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, macos-latest]
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - name: test -race
        env:
          CGO_ENABLED: "1"
        run: go test -race ./...

  fuzz:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - name: gate da SPEC §25.4
        run: go test ./internal/codec -fuzz=FuzzValue -fuzztime=60s
      - name: demais alvos
        run: |
          for f in FuzzRoundTrip FuzzUvarint FuzzKeyEncoding FuzzKeyReader; do
            go test ./internal/codec -run '^$' -fuzz="^${f}\$" -fuzztime=60s
          done

  deps:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - run: go mod verify
      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
      - run: scripts/check-nocgo.sh

  names:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - name: nomes proibidos (SPEC §2.1)
        env:
          PATTERN: ${{ vars.FORBIDDEN_NAMES_REGEX }}
        run: |
          if [ -z "$PATTERN" ]; then
            echo "::error::a variável de repositório FORBIDDEN_NAMES_REGEX não está definida"
            exit 1
          fi
          if git grep -n -I -i -E -e "$PATTERN" -- . ; then
            echo "::error::nome proibido encontrado (SPEC §2.1)"
            exit 1
          fi
````

As actions estão fixadas por SHA (checkout v7.0.1, setup-go v7.0.0). O job `names` lê o padrão proibido da variável de repositório `FORBIDDEN_NAMES_REGEX`, que o autor configura no GitHub; o padrão nunca entra em arquivo versionado.

- [ ] **Step 3: Rodar localmente o que o CI roda**

Run: `scripts/check-nocgo.sh && go mod verify && go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...`

Expected: `nenhum pacote não padrão usa cgo`, `all modules verified` e `No vulnerabilities found.`

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml scripts/check-nocgo.sh
git commit -m "ci: gates da SPEC §25.4 para o M1a"
```


### Task 13: Verificação final do M1a

**Files:**
- Nenhum arquivo novo: só verificação.

- [ ] **Step 1: Formatação e vet**

Run: `gofmt -l . && go vet ./...`

Expected: nenhuma saída.

- [ ] **Step 2: Testes sem cgo**

Run: `CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build ./...`

Expected: `ok` para `tacdb`, `internal/codec`, `internal/errs` e `value`; `internal/valuetest` sem testes.

- [ ] **Step 3: Testes com race**

Run: `CGO_ENABLED=1 go test -race ./...`

Expected: os mesmos `ok`.

- [ ] **Step 4: Nome proibido (passo do autor)**

Este passo precisa do padrão proibido, que só o autor tem (SPEC §2.1); quem não o tiver pula o passo e avisa o autor. A primeira parte do comando aborta se a variável estiver vazia, porque `git grep -e ""` casaria todas as linhas.

Run: `: "${FORBIDDEN_NAMES_REGEX:?peça o padrão ao autor}"; git grep -n -I -i -E -e "$FORBIDDEN_NAMES_REGEX" -- . ; echo "exit=$?"`

Expected: nenhuma linha encontrada e `exit=1`.

- [ ] **Step 5: Atualizar a rastreabilidade**

Nada a mudar se as tarefas acima passaram: o `AUDIT-REGRESSIONS.md` da Task 11 marca R13-11 e R13-17 como concluídos, e R13-10 e R13-13 como parciais (o primeiro CI verde e a variável do GitHub os fecham).

