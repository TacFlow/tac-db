# Compatibility matrix

Policy (§25.1 and §28 of the specification): tacdb aims at functional and semantic parity with the reference engine (R0) in the inventoried capabilities. Every difference has an ID, a reason and a test in this matrix; no divergence disappears without a record.

## 1. Areas

| Area | Policy (§28) | Status |
|---|---|---|
| Property/graph model | Parity, including absence/Null, lists/maps and edge IDs. | Values and codec ready (M1a); graph pending (M3). |
| Cypher dialect of the pinned revision | Parity on the inventoried features. | Pending (M4). |
| FTS/BM25/fuzzy | Functional parity; deterministic scores across platforms. | Pending (M5). |
| HNSW | Same capabilities and metrics; topology is not a binary identity. | Pending (M8). |
| Transactions | One mutating writer; parallel readers; adaptive group commit. | Pending (M2). |
| Physical maintenance | Passive checkpoint, segmented WAL, bitmap allocator, scrub, compact. | Page format and superblock ready (M1c); allocator in M1e; WAL and checkpoint in M2. |
| Cluster/multiprocess | Mandatory in 1.0. | Pending (M10 to M12). |
| Streams | Sequences, offsets, trim and atomicity preserved. | Pending (M6). |
| Security | The §20 model, with no counterpart in R0. | File permissions, paths and locks ready (M1b, see SECURITY.md); the rest pending (M10). |
| R0 main file/WAL | Physically incompatible; migration through LTX1. | By decision (ADR-07). |
| Original Go API | Conformance adapter; new main API. | Pending. |
| C ABI / Python / Java | Outside the initial distribution. | By decision. |
| TypeScript | New SDK over a Go bridge. | Pending (M10, M13). |
| Neo4j-inspired improvements | tacdb's own Go implementation of the §33 contracts, clean room; no binary compatibility, Bolt or Enterprise extension. | Pending (M4 onwards). |
| Memory/serialize | Shared VFS; Serialize produces a consistent G2 snapshot without quiescence. | MemoryVFS ready (M1b); Serialize pending (M9). |
| Embedding helpers/services | Excluded from the runtime; external vectors are optionally accepted. | By decision (ADR-03). |
| TAC | Language 0.6 and IR 1.3 for the database; new constructs are not skill aliases. | Pending (M7). |
| LAYA/SLM constructs and implicit legacy memory | Absent from the profile; an error or an explicit source migration. | Pending (M7). |
| Performance | No promise of automatic improvement; compare and record; write amplification has a gate. | Pending (benchmarks from M2). |
| G2 limits | IDs and sequences up to MaxInt64; fixed limits require a new `format_revision`. | Value limits (M1a) and page limits (M1c) ready. |

## 2. tacdb's own decisions

| ID | Decision | Reason | Test |
|---|---|---|---|
| D-001 | Total order of values: Bool < number < String < Bytes < Vector < List < Map < Null. Int and Float compare by exact numeric value, without converting int64 to float64; NaN comes after every number and ties with any NaN; `-0` ties with `+0`. `Compare == 0` does not imply equal bits (`Int(1)` and `Float(1)` tie). | §5.3 fixes the families and asks for tacdb's own cases where R0 does not fix the behavior. | `value/compare_test.go` |
| D-002 | Canonical codec: the decoder rejects map keys out of order or repeated, and non-minimal uvarints, as `CORRUPT`. | Every value has a single form (§5.3 and §7.1 of specification 1.3); goldens and signatures depend on it. | `internal/codec/value_test.go`, `internal/codec/uvarint_test.go`, `FuzzValue` |

## 3. Recorded divergences from R0

None so far.
