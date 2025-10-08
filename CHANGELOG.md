# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2025/10/07 - d88addf]

### Added
- Resilient attachment download state. The application now creates a temporary `.download_state.json` for each message to track attachment progress, allowing downloads to be resumed safely after an interruption.
- Memory-safe mutex pooling for file handling to prevent unbounded memory growth on very large-scale downloads.

### Changed
- All writes to `metadata.json` are now atomic, using a write-and-rename strategy to prevent file corruption.
- Folder search is now performed with a single, efficient, case-insensitive OData query, significantly improving performance.

### Fixed
- Correctly implemented API rate limiting and retries by configuring the O365 client with the appropriate middleware. This prevents server-side throttling and improves stability.

### Removed
- Removed a deprecated `SaveAttachment` function and an unused `accessToken` parameter from the engine for better code clarity.

## [2025/10/06 - c52f094]

### New feature: Decouple Concurrency Pools

This change addresses a performance bottleneck that occurred when processing with a high degree of parallelism.

- **BREAKING CHANGE**: Replaced the single `-parallel` flag with two more granular flags: `-parallel-processors` and `-parallel-downloads`. This allows for independent tuning of CPU-bound message processing and I/O-bound attachment downloading.
- Refactored the core engine to use two separate semaphore pools, one for processors and one for downloaders, preventing resource contention and improving overall throughput.
- Updated `README.md` and `ARCHITECTURE.md` to document the new flags and provide performance tuning guidance.

### New feature: Streaming Download for Large Attachments

This change implements a high-performance, memory-efficient mechanism for downloading large attachments, which were previously skipped.

- Implemented a dual-path download strategy:
  - Small attachments with inline `contentBytes` are decoded and saved directly.
  - Large attachments are now downloaded by streaming from the `@microsoft.graph.downloadUrl` provided by the API. This keeps memory usage minimal, regardless of file size.
- Modified the O365 client to request only attachment metadata, ensuring the `downloadUrl` is returned for large files.
- Added a new `SaveAttachmentFromURL` function to the file handler to manage streaming the download directly to disk.
- Updated `ARCHITECTURE.md` to document the new streaming architecture.