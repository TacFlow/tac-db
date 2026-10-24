# FORMAT — the tacdb G2 format

This document describes the encodings of the G2 format **as implemented**, with examples that the tests check byte for byte. The normative source is the specification (§5.3, §7 and §9); where the two disagree, the specification wins and the difference is a bug.

The document grows with each milestone. This version covers the value codec, uvarints and composite keys (M1a). The superblock, pages, extents and bitmaps arrive with the `internal/page` package (M1c), and the WAL with M2.

## 1. Conventions

| Item | Rule |
|---|---|
| Payload integers | Little-endian, fixed width. |
| Integers in B+tree keys | Big-endian, so that byte order is numeric order. |
| Variable-length integers | Uvarint (LEB128) in minimal form (§3). |
| Strings | Valid UTF-8, not normalized. |
| Canonical form | Every value has exactly one encoding; the decoder rejects any other. |

## 2. Value codec

Implementation: `internal/codec/value.go`. Checked examples: `internal/codec/testdata/golden/values.golden`.

Every value starts with a 1-byte tag:

| Tag | Type | Payload after the tag |
|---|---|---|
| `0x00` | Null | None. |
| `0x01` | Bool | 1 byte: `0x00` or `0x01`. |
| `0x02` | Int | int64, 8 bytes. |
| `0x03` | Float | The 8 bytes of `math.Float64bits`; NaN payloads, infinities and `-0` are preserved. |
| `0x04` | String | u32 length and the UTF-8 bytes. |
| `0x05` | Bytes | u32 length and the bytes. |
| `0x06` | Vector | u32 dimension (1 to 65535) and the bits of each float32 component (4 bytes). |
| `0x07` | List | u32 count and the values, recursively. |
| `0x08` | Map | u32 count and the pairs `u32 key length + UTF-8 key + value`, in increasing byte order of the key, without repetition. |

Examples (hex):

| Value | Encoding |
|---|---|
| `Int(-1)` | `02 ffffffffffffffff` |
| `Int(2^53+1)` | `02 0100000000002000` |
| `Float(1)` | `03 000000000000f03f` |
| `Float(-0)` | `03 0000000000000080` |
| `String("ação")` | `04 06000000 61c3a7c3a36f` |
| `Vector([1, 2])` | `06 02000000 0000803f 00000040` |
| `Map{"a": true, "origem": "manual"}` | `08 02000000 01000000 61 0101 06000000 6f726967656d 04 06000000 6d616e75616c` |

### 2.1 Decoding

The decoder takes limits (`value.Limits`) and an optional budget (`codec.Budget`).

| Situation | Result |
|---|---|
| Unknown tag, Bool other than 0/1, invalid UTF-8, Vector dimension 0 or above 65535 | `CORRUPT` |
| Map keys out of order or repeated (non-canonical form) | `CORRUPT` |
| Length or count larger than the remaining bytes | `CORRUPT`, before allocating |
| Bytes left after the value | `CORRUPT` |
| Depth above `MaxDepth` (default 64; a scalar has depth 1) | `LIMIT_EXCEEDED` |
| List and Map elements, summed over all levels, above `MaxValueElements` (default 1,000,000) | `LIMIT_EXCEEDED` |
| Map key above `MaxMapKeyBytes` (default 64 KiB) | `LIMIT_EXCEEDED` |
| Vector dimension above `MaxVectorDimensions` (default 4096, at most 65535) | `LIMIT_EXCEEDED` |
| Encoding above `MaxValueBytes` (16 MiB) | `LIMIT_EXCEEDED` |

The budget is charged **before** each allocation: 64 bytes per element, plus the variable bytes of strings, bytes and vectors. An error from the budget stops decoding and is returned unchanged.

## 3. Uvarint

Implementation: `internal/codec/uvarint.go`. This is the LEB128 of `encoding/binary` with one extra rule: the form must be minimal. A uvarint longer than one byte whose last byte is `0x00` is not minimal and is `CORRUPT`, as is a truncated one or one larger than 64 bits.

| Value | Bytes |
|---|---|
| 0 | `00` |
| 127 | `7f` |
| 128 | `80 01` |
| 1024 | `80 08` |
| 2^24 (16 MiB) | `80 80 80 08` |

## 4. Composite keys

Implementation: `internal/codec/key.go`. A B+tree key concatenates components whose byte order is their logical order:

- Unsigned integers: big-endian, fixed width (u8, u16, u32, u64).
- Strings and bytes: each `0x00` becomes `0x00 0xFF`, and the component ends with `0x00 0x00`. So `"a" < "a\x00"` holds for the bytes too (`61 00 00` before `61 00 FF 00 00`), and the terminator separates the component from the next one.
- String prefix: the escaped form **without** the terminator is a prefix of the key of every string that starts with it, for prefix scans.
- End of range: `PrefixEnd(p)` returns the smallest key greater than every key that starts with `p` (it increments the last byte other than `0xFF`); it returns nothing when no such key exists.
- Maximum length of an encoded key: 1024 bytes (`CheckKey`), checked before any mutation.
