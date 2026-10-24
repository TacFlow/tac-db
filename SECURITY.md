# Security

This document records how tacdb implements the security model of §20 of the specification, the differences between platforms and the known limits. It grows with each milestone; this version covers files, paths and locks (M1b).

To report a vulnerability, contact the maintainers of the repository privately; do not open a public issue.

## 1. Files and paths (§20.9)

Implementation: `internal/vfs`.

**Confinement.**
- The path given by the caller is resolved once, following symbolic links. The resulting directory is opened as an `os.Root`, and every later operation stays inside it.
- Inside the database directory, only regular files and directories are accepted. Symbolic links, Windows junctions (which Go reports as `ModeIrregular`), devices, FIFOs and sockets are `UNSAFE_PATH`. `os.Root` on its own would follow a link that stays inside the root, which the specification forbids (R13-03).
- An entry is checked with `Lstat` before it is opened, and the opened file must be the same file (`os.SameFile`); a swap in between is `UNSAFE_PATH`.
- Names must pass `vfs.ValidName` (see FORMAT.md §5).

**Permissions per object (R13-18).**

| Object | Unix | Windows |
|---|---|---|
| Files created by tacdb (main file, lock, segments, temporaries) | Mode 0600, owned by the effective user. | Owner and protected DACL limited to the user, SYSTEM and Administrators. |
| Directories created by tacdb (WAL) | Mode 0700, owned by the effective user. | Same rule. |
| The caller's directory | Owned by the effective user or root; no write access for group or others (0755 passes, 0777 and 1777 do not). | Owned by the user, SYSTEM or Administrators; no effective ACE grants write access to any other SID (CREATOR OWNER is allowed). |

Any violation is `INSECURE_PERMISSIONS`. `AllowSharedDirectory` turns off the owner, mode and ACL checks, never the name and entry-type checks.

**ACLs (R13-21).**
- **macOS.** Extended ACLs are read with `fgetattrlist(ATTR_CMN_EXTENDED_SECURITY)` on the open descriptor, through a raw system call, without cgo. Any *allow* entry that grants write, append, delete, delete-child, attribute, security or ownership rights makes the object insecure, including inheritable entries on the caller's directory. *Deny* entries, such as the "everyone deny delete" of the user folders, never count. Every object tacdb creates is checked right after creation.
- **Windows.** Owner and DACL are read with `GetSecurityInfo` on the open handle. The DACL of every created object is replaced with a protected one; this goes by path (`SetNamedSecurityInfo`), because the handle opened for reading and writing lacks `WRITE_DAC`.
- **Linux.** Mode bits are enough: the mask entry of a POSIX ACL is the group class of the mode, which the rule above already bounds.

**Implementation decision.** The specification counts only entries "for a principal other than the owner". On macOS the owner's GUID cannot be mapped to a uid without cgo, so tacdb counts every allow entry with write rights, the owner's included. A directory with such an entry needs `AllowSharedDirectory`.

## 2. Locks (§8.3, R13-04)

The database lock has three layers:
1. **The `.lock` file**, whose name never changes.
2. **The main file**, which catches hard-link aliases under another name.
3. **An in-process registry** keyed by file identity (device and inode, or volume serial number and file index).

Locks belong to the open file:

| Platform | Mechanism |
|---|---|
| Linux | Open file description locks (`F_OFD_SETLK`). |
| macOS | `flock`. |
| Windows | `LockFileEx` on a byte range at offset 2^62, far beyond any real file size, because Windows locks are mandatory and would otherwise block I/O. |

POSIX record locks are never used: they belong to the process and vanish when any descriptor of the file is closed.

## 3. Durability per platform (§8.3)

| Platform | Sync | SyncData | SyncDir | Rename |
|---|---|---|---|---|
| Linux | `fsync` | `fdatasync` | `fsync` of the directory | `os.Root.Rename` |
| macOS | `F_FULLFSYNC` | `F_FULLFSYNC` | `F_FULLFSYNC`, falling back to `fsync` | `os.Root.Rename` |
| Windows | `FlushFileBuffers` | `FlushFileBuffers` | Documented no-op: NTFS journals directory changes | `MoveFileEx` with `MOVEFILE_WRITE_THROUGH` |

Preallocation (`fallocate`, `F_PREALLOCATE`, `FileAllocationInfo`) never changes the file size, and a failure to preallocate is not an error.

## 4. Known limits

- **Windows.** No Windows machine is used in development: the Windows code is compiled and vetted locally, and its tests (DACL, junctions, locks, renames) run only in CI.
- **Windows renames** go by path (`MoveFileEx`) instead of through `os.Root`. Both names are validated, and the directory was resolved when it was opened.
- **Windows file identity** uses the 64-bit file index of `GetFileInformationByHandle`. On ReFS, 128-bit IDs are truncated, which can only affect the in-process registry; cross-process locks rely on `LockFileEx`.
- **Swaps inside the database directory** need write access to it, which the caller-directory rule denies to everyone but the owner.
