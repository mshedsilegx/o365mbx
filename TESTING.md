# TESTING.md - O365 Mailbox Downloader Testing Strategy

This document provides technical details, requirements, and execution procedures for the `o365mbx` test suite.

## 1. Architecture of the Test Suite

We employ a **Layered Testing Strategy** to isolate logic, validate integrations, and ensure resilience.

### Layer A: Unit Testing (Logic & Coordination)
- **Tooling**: `go.uber.org/mock` (formerly `uber-go/mock`).
- **Objective**: Verify business rules and the producer-consumer orchestration in `engine`.
- **Technical Requirement**: All core components (`O365Client`, `FileHandler`, `EmailProcessor`) must implement interfaces.
- **Mock Generation**: Controlled via `//go:generate` comments. Run `go generate ./...` to update.

### Layer B: Integration Testing (API & Transport)
- **Tooling**: `github.com/jarcoal/httpmock`.
- **Objective**: Validate interactions with Microsoft Graph API without making real network calls.
- **Technical Requirement**: Inject a custom `msgraphsdk.GraphRequestAdapter` into the `O365Client`.
- **Key Scenarios**: $value stream responses, OData delta links, and complex JSON attachment structures.

### Layer C: Resilience Testing (Real-world Network & Realistic Data)
- **Tooling**: **Dev Proxy** (Microsoft).
- **Objective**: Test how the application handles throttling (429), service unavailability (503), high latency, and complex MIME structures.
- **Technical Requirement**: Use the `proxy` build tag to isolate these tests. Requires Dev Proxy to be running locally at `http://127.0.0.1:8000`.
- **Implementation Pattern**: These tests invoke the **full `RunEngine` pipeline** (Engine -> Client -> FileHandler) to verify cross-package orchestration under stress.
   - **Data Hygiene**: All generated test data is isolated to `$env:UTESTS_HOME` to maintain workspace cleanliness.
- **Validation Depth**: Beyond file existence, these tests perform deep inspection of extracted content (e.g., verifying unique "canary" strings in nested attachments).

---

## 2. Technical Requirements & Setup

### Environment Dependencies
- **Go**: 1.21 or higher.
- **Mockgen**: `go install go.uber.org/mock/mockgen@latest`.
- **Dependencies**: Run `go mod tidy` to ensure `testify`, `httpmock`, and `gomock` are available.
- **Dev Proxy**: Required for Resilience tests. Configure with `m365.json`.

### Required Environment Variables (PowerShell)
Set these variables before running the test suite:
```powershell
$env:PROXY_HOME = "d:\inetd\devproxy"
$env:UTESTS_HOME = "$env:TEMP\o365mbx_UTESTS"
$env:PROJECT_HOME = "e:\data\devel\build\code\private\o365mbx"
```

### Critical Constraints for Test Stability
1. **Parallelism**: Unit tests must set `MaxParallelDownloads: 1` in their configurations to prevent race conditions and non-deterministic failures.
2. **Channel Closure**: Mocks for streaming methods (e.g., `GetMessages`) **MUST** defer closing the provided channel to prevent goroutine leaks and deadlocks.
3. **Data Integrity**: Mocked attachments **MUST** have a `Name` set to avoid nil pointer dereferences during logging operations.

---

## 3. List of Tests

| Logical Group | Test Name | Technical Purpose | Success Criteria |
| :--- | :--- | :--- | :--- |
| **Engine** | `TestRunEngine_Basic` | Verify `full` mode pipeline orchestration. | Producer -> Processor -> Handler sequence completes for 1 message. |
| | `TestRunEngine_Incremental` | Validate `RunState` (Delta Link) lifecycle. | `LoadState` -> API Call with Delta -> `SaveState` with new Delta. |
| | `TestRunEngine_RouteMode` | Test `Aggregator` and `MoveMessage` logic. | Message is moved to `ProcessedFolder` on success, `ErrorFolder` on failure. |
| | `TestRunEngine_WithAttachments` | Verify multi-stage attachment download. | `GetMessageAttachments` -> `SaveAttachmentFromBytes` -> `WriteAttachmentsToMetadata`. |
| | `TestRunEngine_GetMessagesError` | Verify engine exit on producer failure. | Engine returns non-nil error if `GetMessages` fails. |
| | `TestRunEngine_SaveMessageError` | Verify engine handles storage failures. | Engine handles and returns errors if `SaveMessage` fails. |
| | `TestRunEngine_AggregatorLogic` | Test internal aggregation pipelines. | Messages correctly flow through aggregator to final folder. |
| | `TestRunEngine_AggregatorError` | Logic: Aggregator failure handling. | Verifies that message failures trigger `SaveError` and update job stats. |
| | `TestRunEngine_MessageTimeout` | Hardening: Message execution timeout. | Verifies that messages exceeding `MaxExecutionTimeMsg` are moved to the `ErrorFolder`. |
| | `TestValidateWorkspacePath` | Security: Prevent path traversal/escapes. | Rejects relative paths, symlinks, and root-level system directories. |
| | `TestValidateWorkspacePath_Extra` | Hardening: Varied workspace scenarios. | Handles critical paths, existing files, and non-empty directories. |
| | `TestRunEngine_ValidationFail` | Logic: Workspace validation failure. | RunEngine exits early if workspace path is invalid. |
| | `TestRunEngine_CreateWorkspaceFail` | Logic: Workspace creation failure. | RunEngine handles errors from FileHandler.CreateWorkspace. |
| | `TestRunEngine_GetMailboxStatsFail` | Logic: Non-fatal stat failure. | Engine continues if initial mailbox stats retrieval fails. |
| | `TestRunEngine_SaveStatusReportFail` | Logic: Non-fatal report failure. | Engine logs but does not exit if status report generation fails. |
| | `TestRunDownloadMode_LoadStateFail` | Logic: State loading error. | runDownloadMode exits if incremental state cannot be loaded. |
| | `TestRunDownloadMode_SourceFolderFail` | Logic: Folder lookup failure. | runDownloadMode exits if custom source folder cannot be found/created. |
| | `TestRunDownloadMode_AttachmentFetchErr` | Logic: Attachment discovery failure. | Handled as a non-fatal error; message moved to Error folder. |
| | `TestRunDownloadMode_QueuingTimeout` | Hardening: Context cancellation race. | Verified that queuing is interrupted safely during message timeouts. |
| | `TestRunDownloadMode_FinalMetadataError` | Logic: Post-download metadata failure. | Verifies that metadata write errors are captured and reported. |
| | `TestRunAggregator_FolderCreationFail` | Logic: Aggregator setup failure. | Aggregator exits if destination folders cannot be resolved. |
| | `TestRunAggregator_MoveMessageFail` | Logic: Relocation failure. | Aggregator logs error but continues if message move fails. |
| | `TestRunAggregator_UnknownMessageID` | Robustness: Unexpected results. | Aggregator handles results for unknown IDs without crashing. |
| **Config** | `TestConfig_SetDefaults` | Validate default value injection. | Empty config fields are populated with constants from `config.go`. |
| | `TestConfig_Validate` | Validate schema and constraints. | Rejects `MaxParallelDownloads < 1` or missing `MailboxName`. |
| | `TestConfig_Validate_ChromiumPaths` | Logic: Browser path validation. | Handles directory paths or non-existent files for ChromiumPath. |
| | `TestConfig_Validate_Ranges` | Logic: Boundary value validation. | Ensures negative retries, bursts, and invalid enums are rejected. |
| | `TestLoadConfig` | Logic: Configuration loading from file. | Validates JSON deserialization and path resolution. |
| **O365Client** | `TestO365Client_GetMessages_httpmock` | Integration: Parse OData Delta responses. | Successfully maps JSON `value` and `@odata.deltaLink` to models. |
| | `TestO365Client_GetAttachmentRawStream_httpmock` | Integration: Handle `$value` binary streams. | `io.ReadCloser` returned and correctly streams bytes to the caller. |
| | `TestO365Client_GetAttachmentRawStream_Complex` | integration: native HTTP branches. | Covers URL parsing and transport cloning logic. |
| | `TestO365Client_GetAttachmentRawStream_FinalBranches` | integration: HTTP status branches. | Handles 404 and request creation failures in raw stream. |
| | `TestO365Client_GetMessageAttachments_httpmock` | Integration: Fetch attachment metadata. | Correctly parses list of attachments for a specific message. |
| | `TestO365Client_MoveMessage_Success` | Integration: Successful move. | Verifies move operation completion. |
| | `TestO365Client_GetOrCreateFolderIDByName_httpmock` | Integration: Folder management. | Handles folder lookup by name and creation if missing. |
| | `TestO365Client_GetOrCreateFolderIDByName_Errors` | Integration: Folder failure paths. | Handles API failures during lookup or creation. |
| | `TestO365Client_GetMailboxHealthCheck_httpmock` | Integration: Aggregate folder metadata. | Calculates `TotalMessages` and `TotalMailboxSize` from folder list. |
| | `TestO365Client_GetMailboxHealthCheck_InboxAndSorting` | Logic: Health check sorting. | Verifies alphabetical sorting and Inbox-specific date retrieval. |
| | `TestO365Client_GetMailboxHealthCheck_NilFields` | Robustness: Nil field handling. | Verifies behavior when Graph returns null for counts or names. |
| | `TestO365Client_GetMessageDetailsForFolder_httpmock` | Integration: Folder content streaming. | Streams message metadata from a specific folder to a channel. |
| | `TestO365Client_GetMessageDetailsForFolder_EdgeCases` | Integration: Missing recipients. | Handles messages with empty From or To fields. |
| | `TestO365Client_GetMessageDetailsForFolder_Pagination` | Integration: multi-page details. | Verifies pagination traversal for message metadata. |
| | `TestO365Client_Errors_httpmock` | Integration: Map OData errors to AppErrors. | HTTP 401/403/429 results in appropriate `apperrors.APIError`. |
| | `TestO365Client_HandleError` | Logic: Internal error wrapper. | Ensures context deadlines and API errors are correctly processed. |
| | `TestO365Client_HandleError_WithStatusCode` | Logic: Detailed error mapping. | Extracts status codes from embedded API errors. |
| | `TestO365Client_ParseFolderSize` | Logic: Data type normalization. | Handles int64, int32, and float64 variants returned by Graph. |
| | `TestStaticTokenAuthenticationProvider` | Auth: Token injection logic. | Verifies static token header injection for Graph requests. |
| | `TestStaticTokenAuthenticationProvider_HeadersNil` | Auth: Lazy initialization. | Verifies header map creation if missing in RequestInformation. |
| | `TestO365Client_GetMessages_Incremental` | Integration: Captured delta link. | Verifies captured delta link persistence in state. |
| | `TestO365Client_GetMessages_Pagination_Errors` | Integration: Handle OData Delta pagination errors. | Handles 500 errors during nextLink traversal or context cancellation. |
| | `TestO365Client_GetMessages_PaginationBranches` | Integration: Multi-page error paths. | Verified that pagination stops on intermediate page failure. |
| | `TestO365Client_GetMessages_NilResponse` | integration: null response branch. | Handles cases where the API returns 200 OK but null body. |
| **FileHandler** | `TestFileHandler_CreateWorkspace` | Security: Workspace initialization. | Verifies directory creation and security checks (symlinks). |
| | `TestFileHandler_CreateWorkspace_Errors` | Logic: Workspace creation fail. | Handles MkdirAll failures (e.g. parent is a file). |
| | `TestFileHandler_SaveMessage` | Storage: Directory & Metadata creation. | Created `body.txt` and `metadata.json` contain valid, expected content using `os.OpenRoot`. |
| | `TestFileHandler_SaveMessage_DetailedPaths` | Logic: Varied body types. | Verifies handling of []byte bodies and Text/PDF format flags. |
| | `TestFileHandler_SaveMessage_HTML` | Logic: Content-type detection. | Files saved with `.html` extension if content contains `<html>`. |
| | `TestFileHandler_SaveFileAttachment` | Logic: Standard file attachment storage. | Saves binary content with correct naming and sequence prefix using `os.OpenRoot`. |
| | `TestFileHandler_SaveItemAttachment_Extractor` | Logic: MIME parsing and nested extraction. | Extracts body and Level 1 attachments from ItemAttachments using `os.OpenRoot`. |
| | `TestFileHandler_SaveItemAttachment_Extractor_Errors` | Logic: Extraction failure paths. | Handles stream errors and invalid MIME headers during extraction. |
| | `TestFileHandler_extractFilesFromEnvelope_WriteError` | Logic: Extraction write fail. | Gracefully handles IO errors during nested file extraction. |
| | `TestFileHandler_SaveAttachment_Large` | Logic: Threshold-based handling. | Large attachments are handled via correct path and memory logic. |
| | `TestFileHandler_WriteAttachmentsToMetadata` | IO: Metadata updates. | Correctly updates `metadata.json` with final attachment list using `os.OpenRoot`. |
| | `TestFileHandler_WriteAttachmentsToMetadata_ReadError` | IO: Update read fail. | Handles cases where metadata.json cannot be reopened for update. |
| | `TestFileHandler_Errors` | Logic: Storage error handling. | Handles permission issues and disk space errors gracefully. |
| | `TestFileHandler_SaveState` | IO: JSON state persistence. | `state.json` is correctly serialized/deserialized with atomic safety (temp file + rename) and `os.OpenRoot`. |
| | `TestFileHandler_SaveState_Errors` | Logic: State write failure. | Handles directory access errors during state saving. |
| | `TestFileHandler_LoadState` | IO: State retrieval. | Loads delta links from disk; returns empty state if missing. |
| | `TestFileHandler_LoadState_Malformed` | IO: Corrupted state. | Returns error on invalid JSON state files. |
| | `TestFileHandler_GetMutex_Concurrency` | Logic: Thread-safe IO. | Verifies internal mutex pooling for safe concurrent file access. |
| | `TestFileHandler_ToRecipient_Complete` | Logic: Edge case recipients. | Handles nil recipients or missing email addresses in Graph models. |
| | `TestSanitizeFileName` | Security: OS-safe filename generation. | Replaces `/ \ : * ? " < > |` and `..` with `_`. |
| | `TestFileHandler_SaveError` | Logic: Per-message error reporting. | Generates `error.json` with timestamps and descriptions on failure. |
| | `TestFileHandler_SaveError_UnmarshalError` | Logic: Appending to invalid JSON. | Handles corrupted error.json files by overwriting instead of failing. |
| | `TestFileHandler_SaveStatusReport` | Logic: Job-level status summary. | Generates root `status_<timestamp>.json` with mailbox snapshots. |
| | `TestFileHandler_SaveStatusReport_MarshalError` | Logic: Report IO failure. | Handles workspace access errors during reporting. |
| **EmailProcessor** | `TestEmailProcessor_IsHTML` | Logic: Detect HTML content. | Correctly identifies strings containing HTML tags. |
| | `TestEmailProcessor_CleanHTML` | Logic: HTML to Markdown conversion. | Verifies sanitization and link/image preservation. |
| | `TestEmailProcessor_CleanHTML_NestedAndStyles` | Logic: Complex HTML layout. | Verified script/style exclusion and nested list handling. |
| | `TestEmailProcessor_ProcessBody` | Conversion: HTML to Text/PDF logic. | Returns clean text or calls Chromium for PDF based on `ConvertBody` setting. |
| | `TestEmailProcessor_Initialize_Errors` | Logic: Browser setup failure. | Handles non-existent Chromium paths and empty paths correctly. |
| | `TestEmailProcessor_ConvertToPDF` | Performance: PDF generation. | Verifies high-fidelity rendering using local browser. |
| | `TestEmailProcessor_ConvertToPDF_ContextCancelled` | Robustness: Context handling. | Verifies immediate exit on cancelled context before browser call. |
| | `TestEmailProcessor_ConvertToPDF_InvalidContent` | Robustness: Browser resilience. | Verifies that empty or malformed content doesn't crash the browser. |
| | `TestEmailProcessor_Close_Nil` | Logic: Graceful shutdown. | Ensures no panics if Close is called on an uninitialized processor. |
| | `TestEmailProcessor_PoolConcurrency` | Performance: Resource isolation. | Verifies that the page pool correctly handles multiple concurrent renders. |
| | `TestEmailProcessor_Recycling` | Reliability: Memory management. | Verifies that the browser instance is recycled after a set number of conversions. |
| **Presenter** | `TestRunHealthCheckMode` | Output: Terminal formatting. | Tabular output contains correct mailbox statistics. |
| | `TestRunHealthCheckMode_MultipleFolders` | Output: Multi-folder formatting. | Verifies correct tabular alignment for varied folder lists. |
| | `TestRunHealthCheckMode_TabwriterError` | robustness: writing warning branch. | Verified error handling when stdout/tabwriter fails. |
| | `TestRunMessageDetailsMode` | Output: Folder content listing. | Correctly displays table of messages for a specific folder. |
| | `TestRunMessageDetailsMode_LongSubject` | output: formatting truncation. | Verifies that subjects over 75 chars are truncated with "...". |
| | `TestRunMessageDetailsMode_TabwriterError` | robustness: writing warning branch. | Verified error handling when stdout/tabwriter fails. |
| | `TestRunMessageDetailsMode_ContextCancelled` | Robustness: Streaming interruption. | Verifies that details streaming stops on context cancellation. |
| **Utils** | `TestStringValue` | Safety: Nil-safe string deref. | Returns fallback value instead of panicking on nil pointers. |
| | `TestTimeValue` | Safety: Nil-safe time deref. | Returns fallback time instead of panicking on nil pointers. |
| | `TestBoolValue` | Safety: Nil-safe bool deref. | Returns fallback bool instead of panicking on nil pointers. |
| | `TestInt32Value` | Safety: Nil-safe int32 deref. | Returns fallback int32 instead of panicking on nil pointers. |
| **AppErrors** | `TestAPIError_Error` | Logic: Error formatting. | Verifies API error message construction. |
| | `TestFileSystemError_Error` | Logic: Error formatting. | Verifies file system error message construction and wrapping. |
| | `TestErrMissingDeltaLink` | Logic: Static error. | Verifies constant error message. |
| **Main** | `TestLoadAccessToken` | Logic: Token source priority. | Validates `-token-string`, `-token-file`, and `-token-env` behavior. |
| | `TestIsValidEmail` | Logic: Email validation. | Rejects malformed email addresses for the `-mailbox` flag. |
| | `TestValidateFinalConfig` | Logic: Cross-field validation. | Ensures mode-specific requirements (e.g., route mode folders) are met. |
| | `TestOverrideConfigWithFlags` | Logic: Configuration layering. | Verifies CLI flags correctly override `config.json` values. |
| | `TestCheckLongPathSupportMock` | Logic: OS capability check. | Verifies that the Windows-specific long path check is executed during startup. |
| | `TestRun/Healthcheck_mode` | Integration: Diagnostics execution path. | Verifies `RunHealthCheckMode` is invoked correctly from CLI. |
| | `TestRun_TokenFileRemoval` | Logic: Deferred cleanup of credential files. | Ensures token files are deleted after application exit. |
| **Resilience** | `TestResilience_DevProxy` | Chaos: Network failure simulation. | Application retries on 429/503 and eventually succeeds or logs error. |
| | `TestResilience_NestedAttachmentExtraction` | Complex Data: Level 1 Recursion. | Extracts body from nested `.eml` and verifies content against known unique strings. |
| | `TestResilience_MassiveAttachmentSet` | Pressure: High IO frequency. | Correctly saves 100+ attachments for a single message without resource leaks. |
| | `TestResilience_ConcurrencyPressure` | Pressure: Full Pipeline orchestration. | `RunEngine` handles 20+ complex messages with simulated network jitter (500ms-2s delay). |
| | `TestResilience_HighFidelity_InlinesEnabled` | Comprehensive Chaos: Mixed, Recursive, Massive & Kitchen Sink data with inlines. | Extracts all parts (including inlines) from mixed, recursive, massive, and 50+ kitchen sink attachment sets under 50% failure. |
| | `TestResilience_HighFidelity_DefaultMode` | Comprehensive Chaos: Mixed, Recursive, Massive & Kitchen Sink data (default). | Validates that inlines are NOT extracted by default while other logic holds for all data types. |
| | `TestResilience_LiveProxyBehavior` | Chaos: Full pipeline synchronization. | Validates Megan Bowen profile and mailbox sync against live proxy with errors. |

---

## 4. Code Coverage Report

The project maintains high testing standards with an overall statement coverage exceeding the 90% target.

| Package | Statement Coverage | Status |
| :--- | :--- | :--- |
| `o365mbx/apperrors` | 100.0% | **PASS** |
| `o365mbx/emailprocessor` | 90.2% | **PASS** |
| `o365mbx/engine` | 94.4% | **PASS** |
| `o365mbx/filehandler` | 87.9% | **PASS** |
| `o365mbx/o365client` | 90.9% | **PASS** |
| `o365mbx/presenter` | 88.5% | **PASS** |
| `o365mbx/utils` | 100.0% | **PASS** |
| `o365mbx` (main) | 84.3% | **PASS** |
| **Project Total** | **~91.0%** | **GOAL MET** |

---

## 5. Realistic Data Simulation (Dev Proxy)

To ensure tests are realistic, we define JSON and MIME mocks for Dev Proxy that simulate complex "real-world" scenarios in a single comprehensive pipeline (`$env:PROJECT_HOME\tests\resilience-full-pipeline.json`):

- **Chaos Simulation**: All resilience tests run against a 50% transient failure rate (429/503) to verify the application's retry logic and data integrity.
- **Message Execution Timeout**: Validates that worker threads release resources and route messages to the `Error` folder if processing (body conversion or attachment download) exceeds the configured time limit.
- **"High-Fidelity" mixed data**: A message containing a mix of `FileAttachment` and `ItemAttachment` types, including Unicode filenames, OS-illegal characters, and zero-byte files.
- **Recursive MIME extraction**: Multiple messages with nested `.eml` files to validate Level 1 recursion, content parsing, and "canary" string verification.
- **Massive Attachment Pressure**: Simulated messages with 100+ attachments to stress-test parallel I/O and metadata aggregation.
- **"The Kitchen Sink" Message**: A single email containing 50+ mixed attachments (images, PDFs, `.eml`, `.msg`) to stress-test the `FileHandler` and metadata writer.
- **Concurrency Pressure**: Orchestration of 20+ complex messages with simulated network jitter to verify pipeline stability under load.
- **Conditional Inline Extraction**: Validates that inline images are only extracted when `attachmentExtractionL1` is set to `inlines`.
- **Throttled Attachment Stream**: Simulates a connection reset or a 429 error triggered midway through a large binary download to verify resume/retry stability.

---

## 6. How to Run the Tests

### Command Summary (PowerShell)

| Action | Command |
| :--- | :--- |
| **Update Mocks** | `go generate ./...` |
| **Run All Unit Tests** | `go test -v ./...` |
| **Run with Coverage** | `go test -v -cover ./...` |
| **Run Package Only** | `go test -v ./engine/` |
| **Run Specific Test** | `go test -v -run TestRunEngine_Basic ./engine/` |
| **Run Resilience Tests** | `go test -v -tags=proxy ./o365client/` |

### Detailed Execution Steps

1. **Clean Environment**:
   ```powershell
   go clean -testcache
   go mod tidy
   ```

2. **Generate Mocks**:
   This step is required if any interfaces in `o365client`, `filehandler`, or `emailprocessor` have changed.
   ```powershell
   go generate ./...
   ```

3. **Execute Core Tests**:
   Runs all tests EXCEPT those tagged with `proxy`.
   ```powershell
   go test -v -cover ./...
   ```

4. **Verify Coverage Target**:
   Generate a report to ensure coverage is >70%.
   ```powershell
   go test -coverprofile=coverage.out ./...
   go tool cover -func=coverage.out
   ```

5. **Run Resilience (Optional)**:
   
   **1. Ensure Dev Proxy is running**
   Start the devproxy process as a background task, in a separate window:
   ```powershell
   Start-Process "$env:PROXY_HOME\devproxy.exe" -ArgumentList "--config-file `"$env:PROXY_HOME\config\m365.json`" --mocks-file `"$env:PROJECT_HOME\tests\resilience-full-pipeline.json`" --record --output json" -RedirectStandardOutput "$env:UTESTS_HOME\devproxy-log.json"
   ```

   **2. Test that the proxy is operational**
   ```powershell
   Invoke-RestMethod -Uri "https://graph.microsoft.com/v1.0/me" -Proxy "http://127.0.0.1:8000" -SkipCertificateCheck
   ```

   **3. Run the tests as needed**
   ```powershell
   go test -v -tags=proxy ./o365client/resilience_test.go
   ```

   **4. Stop the proxy and inspect the logs**
   (because of buffering)
   ```powershell
   Invoke-RestMethod -Uri "http://127.0.0.1:8897/proxy/stopProxy" -Method Post
   Start-Sleep -Seconds 2
   ```

   **5. Inspect the logs**
   ```powershell
   Get-Content "$env:UTESTS_HOME\devproxy-log.json" | Select-String "<string or regexp to be searched>"
   ```

   **6. Clear the logs before starting again the proxy**
   ```powershell
   Remove-Item -Path "$env:UTESTS_HOME\devproxy-log.json" -Force
   ```

6. **Run PDF Conversion Tests (Optional)**:
   The PDF conversion tests are isolated because they require a headless Chromium/Chrome binary. These tests only run if the `PDF_TEST_CHROMIUM_PATH` environment variable is set.

   ```powershell
   # Set the path to your Chrome or Chromium executable
   # Windows
   $env:PDF_TEST_CHROMIUM_PATH = "d:\inet\www\chromium\bin\chrome.exe"
   # Linux
   export PDF_TEST_CHROMIUM_PATH="/var/opt/chromium/chrome"

   # Run all PDF-related tests in the emailprocessor package
   go test -v -run "TestEmailProcessor_(ConvertToPDF|PoolConcurrency|Recycling)" ./emailprocessor/
   ```


---

## 7. Maintenance & Troubleshooting

- **Test Timeouts**: If tests fail with `panic: test timed out`, check if a mock for a streaming method (like `GetMessages`) is missing a `close(channel)` call.
- **Build Failures**: Ensure `mockgen` is installed and in your `$PATH`.
- **Coverage Gaps**: Focus on adding branch tests for error conditions in `filehandler.go` and `engine.go`.
