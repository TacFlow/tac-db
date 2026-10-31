# FORMAT — the tacdb G2 format

This document describes the encodings of the G2 format **as implemented**, with examples that the tests check byte for byte. The normative source is the specification (§5.3, §7 and §9); where the two disagree, the specification wins and the difference is a bug.

The document grows with each milestone. This version covers the value codec, uvarints and composite keys (M1a), the database files (M1b), and pages, extents, bitmaps and the superblock (M1c). The WAL arrives with M2.

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

## 5. Database files

Implementation: `internal/vfs`. A database `name.tacdb` lives in one directory and uses these names (§7.1):

| Name | Content |
|---|---|
| `name.tacdb` | Main file. |
| `name.tacdb.lock` | Lock file: created empty on first open and never removed, so the lock name never changes. |
| `name.tacdb-wal/` | Directory of the WAL segments (M2), each named `NNNNNNNNNNNNNNNN.wal` with 16 hexadecimal digits. |
| Temporaries (M2) | Always in the destination directory, with the database prefix and a random `crypto/rand` suffix. |

Every name goes through `vfs.ValidName`: one or two components separated by `/`, each starting with a letter or digit, made only of letters, digits, `.`, `_` and `-`, not ending with `.`, and not a reserved Windows device name (`CON`, `NUL`, `COM1` and the like). Any other name is `UNSAFE_PATH`.

The database lock has three layers (§8.3, R13-04): the `.lock` file, the main file itself (which catches hard-link aliases, whose `.lock` file is another one) and an in-process registry keyed by file identity. The exclusive lock belongs to the owner; read-only processes share the shared lock while there is no owner.

## 6. Pages

Implementation: `internal/page`. Golden images: `internal/page/testdata/golden/*.bin`. Pages have 4096 bytes; page `N` lives at byte `N*4096` of the main file. Pages 0 and 1 are the two superblock copies; page 2 is the bitmap of the first allocation group.

Every page starts with a 48-byte header:

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 1 | Type: free 0, internal 1, leaf 2, overflow 3, bitmap 4, fixed 5. |
| 1 | 1 | Flags: bit 0 = PREFIX on slotted pages; zero elsewhere. |
| 2 | 2 | `header_bytes` = 48. |
| 4 | 4 | CRC32C (Castagnoli) of the whole page, computed with this field zeroed. |
| 8 | 8 | The page's own ID. |
| 16 | 8 | CommitID of this version of the page. |
| 24 | 2 | Slots of a slotted page (the prefix slot included); useful bytes of an overflow or fixed page; zero otherwise. |
| 26 | 2 | `lower`: end of the slot array of a slotted page. |
| 28 | 2 | `upper`: start of the cell area of a slotted page. |
| 30 | 2 | Level: 0 for a leaf, at least 1 for an internal page. |
| 32 | 8 | Next leaf (leaf), group number (bitmap) or index ID (fixed). |
| 40 | 8 | First child (internal). |

Fields a type does not use are zero. A page read from storage goes through `page.Verify` before any other code sees it: CRC, header, the structure of its type and every page reference it holds (against `pageCount`). `page.Canonical` additionally checks the canonical form; the structural check (§11) uses it.

## 7. Slotted pages (leaf and internal)

Slots of 4 bytes (`offset u16`, `length u16`) follow the header, in key order, so `lower = 48 + 4*slots`. In canonical form, the cells are packed in slot order right after `upper`, and the last one ends at byte 4096; the space between `lower` and `upper` is zero.

**Prefix (R13-19).** Let `p` be the longest common prefix of the first and last full key. The PREFIX flag is set if and only if the page has at least two keys, `p >= 1`, and the encoding with prefix `p` is strictly smaller than the encoding without a prefix. When it is set, slot 0 holds exactly those `p` bytes and every cell stores only the rest of its key. The canonical size of a set of cells is the size of this form, and it is never larger than the size with any other prefix choice. Every fit decision uses it.

**Leaf cell.** `flags u8 | uvarint(suffix length) | uvarint(value length) | key suffix | payload`.

| Flags | Payload |
|---|---|
| `0x00` | The value itself (inline). |
| `0x01` | `extent count u8` (1 to 4), then that many extents of `start u64 + pages u32`. |
| `0x03` | `descriptor u64`: an overflow page listing 5 to 337 extents. |

**Inline rule (R13-01, R13-15).** A value is inline if and only if `1 + uvlen(len(key)) + uvlen(len(value)) + len(key) + len(value) <= 1344`, with the full key. The measure does not depend on the page prefix. With it, each cell and its slot take at most `(4048 - 4) / 3 = 1348` bytes, so any full page plus one more cell splits into two pages; a leaf holds at least three cells, and the worst out-of-page cell (1024-byte key, four extents) takes 1080 bytes.

**Extents.** A value out of the page needs `ceil(length / 4048)` overflow pages. Up to 4 extents stay inline; 5 to 337 go to a descriptor page; never more. Extents follow the order of the value's bytes, each has at least one page, and two consecutive extents are never physically contiguous (they would be one).

**Internal cell.** `uvarint(separator suffix length) | right child u64 | separator suffix`. The first child is the header's `aux` field. Child 0 holds keys below separator 0; the right child of separator `i` holds keys from separator `i` up to (not including) separator `i+1`.

Example (golden `leaf-prefix.bin`): keys `user/0001` and `user/0002` share the 8-byte prefix `user/000`; with it the page uses 84 bytes, without it 88, so the prefix is used. Slot 0 points to `user/000`, slot 1 to `00 01 05 31 616c696365` (inline, suffix `1`, value `alice`), slot 2 to `00 01 03 32 626f62`.

## 8. Other pages

| Type | Content |
|---|---|
| Overflow | Up to 4048 useful bytes from offset 48; `count` gives their number; the rest is zero. Every page of a value holds 4048 bytes except the last. |
| Descriptor | An overflow page whose useful bytes are 5 to 337 extents of 12 bytes; `count = 12 * n`. |
| Bitmap | Group `g` has its bitmap at page `2 + g*32385` and covers the 32384 pages after it; the header's offset 32 holds `g`. The page of index `i` in the group (`i = PageID - bitmap - 1`) is bit `i % 8`, counting from the least significant, of byte `48 + i/8`; 1 means allocated. |
| Free | What the scrub leaves behind: type 0, with `header_bytes`, CRC, page ID and CommitID set and every other byte zero. |
| Fixed | Vector storage (M8); this milestone checks only the header. |

## 9. Pointers

A page reference is valid only if it is not 0, not 1 (a superblock), not a bitmap page and below `pageCount` (`page.CheckPointer`, R13-05). An extent is valid only if it is not empty, ends at or below `pageCount` and covers no superblock or bitmap page (`page.CheckExtent`).

## 10. Superblock

Pages 0 and 1 hold two copies; a checkpoint writes the copy with the older generation.

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 8 | Magic `TACDBG2\0`. |
| 8 | 2 | `format_version` = 2. |
| 10 | 2 | `min_reader_version` = 2. |
| 12 | 4 | `page_size` = 4096. |
| 16 | 16 | Database UUID. |
| 32 | 8×12 | Generation, `checkpoint_lsn`, CommitID, schema epoch, node count, edge count, NodeID high-watermark, EdgeID high-watermark, SymbolID high-watermark, `pageCount`, `wal_floor_segment`, flags (bit 0 = cluster). |
| 128 | 8×32 | Tree roots; 19 to 31 are reserved and zero. |
| 384 | 8 | `free_page_count`. |
| 392 | 8×2 | Applied Raft index and term (zero in standalone). |
| 408 | 2 | `format_revision` = 13. |
| 410 | 3682 | Reserved, zero. |
| 4092 | 4 | CRC32C of bytes 0..4091. |

**Choosing a copy (R13-20).** Each copy is classified in this order: *foreign* if the magic differs; *damaged* if the CRC is wrong; *unsupported* if `format_version` is not 2, `format_revision` is not 13 or `min_reader_version` is above 2; *damaged* if any other field is invalid (`min_reader_version` below 2, page size, unknown flags, non-zero reserved bytes or roots 19..31, `pageCount` below 2 or too large, `free_page_count` above `pageCount`, a root that is not a valid pointer); *valid* otherwise. Then: any unsupported copy, or two foreign copies, make the file `UNSUPPORTED_FORMAT`; between valid copies the higher generation wins, and equal generations with different bytes are `CORRUPT`; with no valid copy the file is `CORRUPT`. Future formats keep the magic, the version fields and the CRC at these offsets so that this reader recognizes them as unsupported.

## 11. Trees and allocation

Implementation: `internal/btree`, `internal/alloc`, `internal/check`. `btree.Verify` checks one tree and `check.Structural` the whole state; a valid state follows these rules (§8.1, §8.4):

- **Roots.** An empty tree has root 0. A root is a leaf or an internal page with at least one separator; a root with one child collapses into it.
- **Levels.** A leaf has level 0, and each child of an internal page of level `L` has level `L - 1`. A tree has at most 40 levels.
- **Keys and separators.** Keys strictly increase within a page. Every key of child `i` lies in `[separator i-1, separator i)`, so each separator `s` between a left subtree `L` and a right subtree `R` satisfies `max(L) < s <= min(R)`.
- **Leaf chain.** The next-leaf field links the leaves in key order; the last one holds 0.
- **Ownership.** Every tree page, overflow page and descriptor page belongs to exactly one tree, in one place.
- **Bitmaps.** A bit is set if and only if its page is owned. Bits of pages at or beyond `pageCount` are zero, and `free_page_count` is the number of clear bits of data pages below `pageCount`.

The placement of pages and the choice of separators are policy, not format: a reader accepts any state that follows the rules above. tacdb splits a leaf with the shortest valid separator, `min(R)[:lcp(max(L), min(R)) + 1]`, and an internal page by moving its middle separator up; merges and redistributions keep the separators valid but not necessarily shortest. tacdb takes a new page from the lowest free address of the group of a related page (a sibling or the parent), then from the lowest free address, then from the end of the file, where reaching the position of the next bitmap page creates that group. A value takes one contiguous free run when there is one; otherwise free runs from the lowest address, completed at the end of the file; and when that would take more than 337 extents, one run at the end of the file, broken only by bitmap pages (R13-02). Freeing a page only clears its bit; `pageCount` does not shrink.
