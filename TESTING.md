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
| | `TestValidateWorkspacePath` | Security: Prevent path traversal/escapes. | Rejects relative paths, symlinks, and root-level system directories. |
| **Config** | `TestConfig_SetDefaults` | Validate default value injection. | Empty config fields are populated with constants from `config.go`. |
| | `TestConfig_Validate` | Validate schema and constraints. | Rejects `MaxParallelDownloads < 1` or missing `MailboxName`. |
| | `TestConfig_ValidateChromiumPath` | Validate Chromium/Chrome existence. | Ensures `ChromiumPath` points to a valid executable file. |
| | `TestLoadConfig` | Logic: Configuration loading from file. | Validates JSON deserialization and path resolution. |
| **O365Client** | `TestO365Client_GetMessages_httpmock` | Integration: Parse OData Delta responses. | Successfully maps JSON `value` and `@odata.deltaLink` to models. |
| | `TestO365Client_GetAttachmentRawStream_httpmock` | Integration: Handle `$value` binary streams. | `io.ReadCloser` returned and correctly streams bytes to the caller. |
| | `TestO365Client_GetMessageAttachments_httpmock` | Integration: Fetch attachment metadata. | Correctly parses list of attachments for a specific message. |
| | `TestO365Client_MoveMessage_httpmock` | Integration: Message relocation. | Verifies correct POST request to Graph `move` endpoint. |
| | `TestO365Client_GetOrCreateFolderIDByName_httpmock` | Integration: Folder management. | Handles folder lookup by name and creation if missing. |
| | `TestO365Client_GetMailboxHealthCheck_httpmock` | Integration: Aggregate folder metadata. | Calculates `TotalMessages` and `TotalMailboxSize` from folder list. |
| | `TestO365Client_GetMessageDetailsForFolder_httpmock` | Integration: Folder content streaming. | Streams message metadata from a specific folder to a channel. |
| | `TestO365Client_Errors_httpmock` | Integration: Map OData errors to AppErrors. | HTTP 401/403/429 results in appropriate `apperrors.APIError`. |
| | `TestO365Client_HandleError` | Logic: Internal error wrapper. | Ensures context deadlines and API errors are correctly processed. |
| | `TestStaticTokenAuthenticationProvider` | Auth: Token injection logic. | Verifies static token header injection for Graph requests. |
| | `TestO365Client_GetMailboxStats_httpmock` | Integration: Snapshot folder item counts. | Successfully maps Graph API response to a folder-to-count map. |
| **FileHandler** | `TestFileHandler_CreateWorkspace` | Security: Workspace initialization. | Verifies directory creation and security checks (symlinks). |
| | `TestFileHandler_SaveMessage` | Storage: Directory & Metadata creation. | Created `body.txt` and `metadata.json` contain valid, expected content. |
| | `TestFileHandler_SaveMessage_HTML` | Logic: Content-type detection. | Files saved with `.html` extension if content contains `<html>`. |
| | `TestFileHandler_SaveFileAttachment` | Logic: Standard file attachment storage. | Saves binary content with correct naming and sequence prefix. |
| | `TestFileHandler_SaveItemAttachment_Extractor` | Logic: MIME parsing and nested extraction. | Extracts body and Level 1 attachments from ItemAttachments. |
| | `TestFileHandler_SaveAttachment_Large` | Logic: Threshold-based handling. | Large attachments are handled via correct path and memory logic. |
| | `TestFileHandler_SaveAttachment_Deprecated` | Logic: Deprecation check. | Rejects calls to deprecated SaveAttachment method. |
| | `TestFileHandler_WriteAttachmentsToMetadata` | IO: Metadata updates. | Correctly updates `metadata.json` with final attachment list. |
| | `TestFileHandler_Errors` | Logic: Storage error handling. | Handles permission issues and disk space errors gracefully. |
| | `TestFileHandler_State` | IO: JSON state persistence. | `state.json` is correctly serialized/deserialized with atomic-like safety. |
| | `TestSanitizeFileName` | Security: OS-safe filename generation. | Replaces `/ \ : * ? " < > |` and `..` with `_`. |
| | `TestFileHandler_SaveError` | Logic: Per-message error reporting. | Generates `error.json` with timestamps and descriptions on failure. |
| | `TestFileHandler_SaveStatusReport` | Logic: Job-level status summary. | Generates root `status_<timestamp>.json` with mailbox snapshots. |
| **EmailProcessor** | `TestEmailProcessor_IsHTML` | Logic: Detect HTML content. | Correctly identifies strings containing HTML tags. |
| | `TestEmailProcessor_CleanHTML` | Logic: HTML to Markdown conversion. | Verifies sanitization and link/image preservation. |
| | `TestEmailProcessor_ProcessBody` | Conversion: HTML to Text/PDF logic. | Returns clean text or calls Chromium for PDF based on `ConvertBody` setting. |
| **Presenter** | `TestRunHealthCheckMode` | Output: Terminal formatting. | Tabular output contains correct mailbox statistics. |
| | `TestRunMessageDetailsMode` | Output: Folder content listing. | Correctly displays table of messages for a specific folder. |
| **Utils** | `TestStringValue` | Safety: Nil-safe string deref. | Returns fallback value instead of panicking on nil pointers. |
| | `TestTimeValue` | Safety: Nil-safe time deref. | Returns fallback time instead of panicking on nil pointers. |
| | `TestBoolValue` | Safety: Nil-safe bool deref. | Returns fallback bool instead of panicking on nil pointers. |
| | `TestInt32Value` | Safety: Nil-safe int32 deref. | Returns fallback int32 instead of panicking on nil pointers. |
| **AppErrors** | `TestAPIError_Error` | Logic: Error formatting. | Verifies API error message construction. |
| | `TestFileSystemError_Error` | Logic: Error formatting. | Verifies file system error message construction and wrapping. |
| | `TestErrMissingDeltaLink` | Logic: Static error. | Verifies constant error message. |
| **Main** | `TestLoadAccessToken` | Logic: Token source priority. | Validates `-token-string`, `-token-file`, and `-token-env` behavior. |
| | `TestIsValidEmail` | Logic: Email validation. | Rejects malformed email addresses for the `-mailbox` flag. |
| | `TestCLIOverrides` | Logic: Configuration layering. | Verifies CLI flags correctly override `config.json` values. |
| | `TestCheckLongPathSupportMock` | Logic: OS capability check. | Verifies that the Windows-specific long path check is executed during startup. |
| | `TestSignalHandling` | Logic: Process lifecycle. | Verifies that the application initiates graceful shutdown on SIGINT/SIGTERM. |
| **Resilience** | `TestResilience_DevProxy` | Chaos: Network failure simulation. | Application retries on 429/503 and eventually succeeds or logs error. |
| | `TestResilience_NestedAttachmentExtraction` | Complex Data: Level 1 Recursion. | Extracts body from nested `.eml` and verifies content against known unique strings. |
| | `TestResilience_MassiveAttachmentSet` | Pressure: High IO frequency. | Correctly saves 100+ attachments for a single message without resource leaks. |
| | `TestResilience_ConcurrencyPressure` | Pressure: Full Pipeline orchestration. | `RunEngine` handles 20+ complex messages with simulated network jitter (500ms-2s delay). |
| | `TestResilience_HighFidelity_InlinesEnabled` | Comprehensive Chaos: Mixed, Recursive, Massive & Kitchen Sink data with inlines. | Extracts all parts (including inlines) from mixed, recursive, massive, and 50+ kitchen sink attachment sets under 50% failure. |
| | `TestResilience_HighFidelity_DefaultMode` | Comprehensive Chaos: Mixed, Recursive, Massive & Kitchen Sink data (default). | Validates that inlines are NOT extracted by default while other logic holds for all data types. |
| | `TestResilience_LiveProxyBehavior` | Chaos: Full pipeline synchronization. | Validates Megan Bowen profile and mailbox sync against live proxy with errors. |

---

## 4. Code Coverage Report

The project maintains high testing standards with an overall statement coverage exceeding the 80% target.

| Package | Statement Coverage | Status |
| :--- | :--- | :--- |
| `o365mbx/apperrors` | 100.0% | **PASS** |
| `o365mbx/emailprocessor` | 75.3% | **PASS** |
| `o365mbx/engine` | 85.4% | **PASS** |
| `o365mbx/filehandler` | 79.4% | **PASS** |
| `o365mbx/o365client` | 81.4% | **PASS** |
| `o365mbx/presenter` | 82.7% | **PASS** |
| `o365mbx/utils` | 100.0% | **PASS** |
| `o365mbx` (main) | 72.1% | **PASS** |
| **Project Total** | **~83.6%** | **GOAL MET** |

---

## 5. Realistic Data Simulation (Dev Proxy)

To ensure tests are realistic, we define JSON and MIME mocks for Dev Proxy that simulate complex "real-world" scenarios in a single comprehensive pipeline (`$env:PROJECT_HOME\tests\resilience-full-pipeline.json`):

- **Chaos Simulation**: All resilience tests run against a 50% transient failure rate (429/503) to verify the application's retry logic and data integrity.
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
   Utilize skill: `invoke-devproxy-o365`

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
   The PDF conversion test is isolated to prevent unexpected security alerts. It only runs if a valid Chromium path is provided.

   ```powershell
   $env:PDF_TEST_CHROMIUM_PATH = "C:\Program Files\Google\Chrome\Application\chrome.exe" # Replace with your path
   go test -v -run TestEmailProcessor_ConvertToPDF ./emailprocessor/
   ```


---

## 7. Maintenance & Troubleshooting

- **Test Timeouts**: If tests fail with `panic: test timed out`, check if a mock for a streaming method (like `GetMessages`) is missing a `close(channel)` call.
- **Build Failures**: Ensure `mockgen` is installed and in your `$PATH`.
- **Coverage Gaps**: Focus on adding branch tests for error conditions in `filehandler.go` and `engine.go`.
