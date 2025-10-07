# Changelog

All notable changes to this project will be documented in this file.

## [git hash to be populated later]

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