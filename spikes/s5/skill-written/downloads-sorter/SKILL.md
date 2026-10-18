---
name: downloads-sorter
description: Sort Downloads folder into PDFs, images, and other types with SORTED.md summary.
version: 0.1.0
author: Hermes Agent
license: MIT
platforms: [macos]
metadata:
  hermes:
    tags: [productivity, downloads-sorting, file-management]
    related_skills: []
---

# Downloads Sorter Skill

A macOS utility for sorting a Downloads folder by file type into categorized subdirectories and generating a markdown summary report. Handles PDFs (`.pdf`), images (PNG, JPG, GIF, WEBP, BMP, TIFF), documents (`.doc`, `.docx`, `.txt`, `.md`), archives, and other formats via `terminal` file system operations.

## When to Use

- **Downloads cluttered**: Need to clean organized Downloads folder on macOS
- **File type separation**: Automatically categorize mixed-type files by extension
- **Audit trail required**: Generate `SORTED.md` documenting moved/filereported files (timestamp, source, destination)
- **Bulk file triage**: Handle 10–1000+ files without manual drag-and-drop

**Don't use for:**
- Network shares or non-macOS platforms (no native macOS terminal integration)
- Real-time auto-sorting (batch operation runs once per call)
- Replacing existing folder schemas

## Prerequisites

- macOS with Terminal.app accessible
- Downloads directory exists at user's home folder (`~/Downloads` by default)
- `terminal`, `read_file`, `write_file`, `search_files` tools available

## How to Run

```bash
terminal(command="python ~/Downloads/DownloadsSorter.py", timeout=60)
```

Or via Hermes:
```bash
terminal(command="hermes-cli downloads-sorter", timeout=60)
```

## Quick Reference

| Step | Action | Tool |
|------|--------|------|
| Initialize folder structure | Create default subdirs | `terminal` + `os.makedirs()` |
| Scan Downloads | Enumerate files and extensions | `search_files` in Python code |
| Categorize files | Match extension rules to folders | Python logic in script |
| Move files | Copy/move by basename, keep metadata | `shutil.move` / `shutil.copy2` |
| Record movements | Write SORTED.md with file logs | `write_file` |

## Procedure

1. Verify Downloads folder exists; default path is `~/Downloads`
   - **Criterion**: `os.path.isdir(~/Downloads)` returns `True`
   - If missing, ask user for custom path before proceeding

2. Identify file types to categorize (extension-based rules):
   - PDFs: any `.pdf` extension
   - Images: `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.bmp`, `.tiff`, `.tif`
   - Documents: `.doc`, `.docx`, `.txt`, `.md`, `.html`, `.htm`
   - Archives: `.zip`, `.rar`, `.tar.gz`, `.tgz`, `.7z`
   - Executables: `.exe`, `.msi`, `.dmg`
   - Default fallback: all else goes to `Other` folder

3. Create destination folders (if they don't exist):
   ```python
   terminal(command="python 'create_folder_structure()'")
   ```
   - Target folder tree: `~/Downloads/{PDFs,Images,Documents,Arcives,Executables,Other}`
   - **Criterion**: All six subdirectories exist with correct names

4. Enumerate all files in Downloads directory (exclude symlinks):
   ```python
   terminal(command="python 'list_downloads_files()'")
   ```
   - Use `os.scandir()` on Downloads, skip symlink entries
   - **Criterion**: Returns file list excluding `.` and `..` directories

5. Categorize each file by extension:
   ```python
   terminal(command="python 'categorize_by_extension(file_path)'")
   ```
   - Map extensions to destination folders via rules defined in step 2
   - **Criterion**: Each file assigned one category (may fall into default folder)

6. Move files to appropriate destinations:
   ```python
   terminal(command="python 'move_files_to_folders(foreground=True)'")
   ```
   - Copy files first to avoid losing originals, then rename/move if preferred
   - Preserve file metadata (`copy2` preserves permissions/time)
   - **Criterion**: All files in destination folders; source count unchanged

7. Write SORTED.md with operation log:
   ```python
   terminal(command="python 'write_sort_log()'")
   ```
   - Generate markdown documenting each affected file
   - Entry format: `### <filename>` with subsections for Source, Destination, DateModified, Action taken
   - Include summary counts by category at top of SORTED.md
   - **Criterion**: SORTED.md exists in Downloads folder with valid markdown structure

8. Verify sort completion:
   ```python
   terminal(command="python 'verify_file_counts()'")
   ```
   - Cross-check source and destination totals match expectations
   - Confirm no files were skipped or duplicated
   - **Criterion**: All operations logged successfully, counts consistent

## Pitfalls

1. **Symlink confusion**: macOS may create symlinks in Downloads; these are NOT moved but logged separately
   - Solution: Exclude from move list, track with note in SORTED.md under "Moved" section

2. **Hidden files**: Files starting with `.` are typically hidden; consider whether to sort them
   - Solution: Option to include/exclude dotfiles; default excludes unless explicitly specified

3. **Large file sizes**: Moving very large files can take significant time
   - Solution: Add timeout handling and progress reporting for >100MB files

4. **Permissions issues**: Users outside admin group need write access to Downloads
   - Solution: Use appropriate permissions (`-rw-r--r--`); may require sudo if Downloads is mounted read-only

5. **Original file loss**: Decide whether copy-before-move strategy is appropriate for each case
   - Solution: Default is safe copy; allow user preference override per-call

6. **Extension parsing quirks**: Windows/Windows-encoded filenames may have mojibake extensions
   - Solution: Use `file --interactive-type` to verify actual file type when uncertain

7. **Empty files**: Empty files with misleading extensions need review before moving
   - Solution: Option to filter or include zero-byte files; default includes all for completeness

## Verification

Run sort once, then confirm:
```bash
terminal(command="python 'report_sort_completion()'")
```
- Check SORTED.md exists as markdown with valid syntax
- Confirm file counts in source directory dropped by expected amount
- Verify destination folders have correct subfolder structure
- No files unaccounted for or double-counted
