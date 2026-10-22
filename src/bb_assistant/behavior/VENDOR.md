# Behavior graph implementation

The modules in `vendor/` are an adapted implementation kept inside this package so the Python compatibility workflow can run without importing code from another checkout. They are maintained with this repository and covered by the project license in the repository root.

The adapter in `caido_adapter.py` translates normalized Caido exchanges into bounded graph imports. It creates redacted receipts and graph records; it does not send requests to targets. The parser handles message bytes in memory and marks unknown or truncated input as incomplete. Graph relationships describe observed evidence, not causality or a confirmed finding.

When changing this code, preserve project isolation, provenance checks, transaction boundaries, and explicit incomplete states. Raw captures and receipt data belong only in private runtime storage, never in source fixtures or documentation.
