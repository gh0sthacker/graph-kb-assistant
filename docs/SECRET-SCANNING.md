# Secret scanning

CI runs Gitleaks over the checked-out repository files on pull requests and
pushes to `main`. The scanner image is pinned by digest and runs locally in the
GitHub runner; findings are redacted in its output. It does not upload source
files to a scanning service.

`.gitleaksignore` contains only fingerprints for existing secret-like values
in test fixtures. These are narrow exceptions, not ignored test directories.
Review any new finding before adding an exception. Real credentials must be
removed and rotated; do not add them to the ignore list.

The CI job scans the current checkout, not deleted content in Git history. If a
credential may have been committed and later removed, use a Gitleaks Git-history
scan and rotate it even when the current-file check passes.
