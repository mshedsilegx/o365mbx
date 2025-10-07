# O365 Mailbox Downloader

`o365mbx` is a command-line application written in Go to download emails and attachments from Microsoft Office 365 (O365) mailboxes using the Microsoft Graph API. It leverages the official Microsoft Graph SDK for Go to ensure reliable and efficient communication with the API.

It is designed for high-performance, parallelized downloading and is robust and efficient, with features for handling large mailboxes, large attachments, and transient network errors.

## Features

*   **High-Performance Parallel Processing**: Utilizes a decoupled producer-consumer architecture with independent worker pools for message processing (CPU-bound) and attachment downloading (I/O-bound). This prevents resource contention and maximizes throughput.
*   **Memory-Efficient Streaming Downloads**: Handles attachments of any size with a minimal memory footprint. Small attachments are handled inline, while large attachments are streamed directly from the server to disk, avoiding out-of-memory errors on multi-gigabyte files.
*   **Resumable Attachment Downloads**: If a download is interrupted, the application automatically resumes from where it left off for each message, preventing data loss and saving significant time and bandwidth.
*   **Guaranteed Data Integrity**: Critical metadata files are written atomically using a "write-and-rename" strategy, which prevents file corruption even if the application crashes.
*   **Incremental Mailbox Sync**: Performs efficient incremental synchronization using Microsoft Graph delta queries, fetching only new or changed items on subsequent runs.
*   **Robust API Interaction**: Implements a configurable client-side rate limiter and retry mechanism to gracefully handle API throttling and transient network errors, ensuring stable performance.
*   **Flexible Configuration**: Supports configuration via both a JSON file and command-line arguments, with arguments overriding file settings.
*   **Bandwidth Limiting**: Allows throttling of download bandwidth to avoid hitting data egress limits.
*   **Secure Token Management**: Provides multiple, mutually exclusive options for securely supplying the access token, including auto-removal of token files.
*   **Health Check Mode**: Provides a "health check" mode to verify connectivity, authentication, and mailbox statistics without performing a full download.
*   **Advanced Body Conversion**: Can convert email bodies from HTML to clean plain text or searchable PDFs, with fine-grained control over the underlying browser engine.
*   **Structured Logging**: Uses `logrus` for structured and informative logging, with a configurable debug level.

    > **Security Warning:** The `-rod` flag passes arguments directly to the underlying browser engine. Use this feature with caution and only with trusted arguments to avoid potential command injection vulnerabilities.

## System Requirements

### File Path Length

The application now supports a maximum file path length of 512 characters. On systems where this limit might be an issue (e.g., older Windows versions without long path support), please see the requirements below.

### Windows Long Path Support

Due to the way email subjects and attachment names can create very long file paths, `o365mbx` requires that "Win32 long path" support is enabled on Windows systems. The application will check for this at startup and will exit with an error if it is not enabled.

You can enable this feature using one of the following methods:

**Using the Group Policy Editor (`gpedit.msc`):**
1.  Navigate to: `Local Computer Policy` -> `Computer Configuration` -> `Administrative Templates` -> `System` -> `Filesystem`.
2.  Find and enable the "Enable Win32 long paths" option.

**Using the Registry Editor (`regedit.exe`):**
1.  Navigate to: `HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\FileSystem`.
2.  Set the value of `LongPathsEnabled` (a `DWORD` type) to `1`.

A system restart may be required for the change to take effect.

## Workspace Directory Structure

The application saves each email into a dedicated folder within the specified workspace. The folder is named after the message's unique ID. Here is an example of the directory structure for a single downloaded email:

```
/path/to/your/workspace/
└── AAMkAGI0ZD... (message ID)
    ├── attachments
    │   ├── 01_quarterly_report.pdf
    │   └── 02_logo.png
    ├── body.html
    └── metadata.json
```

*   **`metadata.json`**: A JSON file containing detailed metadata about the email, including sender, recipients, subject, date, and information about the attachments.
*   **`body.html` / `body.txt` / `body.pdf`**: The body of the email. The extension depends on the original content type or the conversion option specified.
*   **`attachments/`**: A sub-directory containing all attachments from the email. Each attachment is prefixed with a two-digit sequence number.

## Metadata JSON Specifications

The `metadata.json` file provides a detailed overview of the downloaded email.

### Field Descriptions

*   **`to`**: A list of recipients in the "To" field. Each recipient object contains an `emailAddress` object with `name` and `address`.
*   **`cc`**: A list of recipients in the "Cc" field. Same structure as `to`.
*   **`from`**: The sender of the email. Same structure as a recipient object.
*   **`subject`**: The subject line of the email.
*   **`received_date`**: The date and time the email was received, in ISO 8601 format (UTC).
*   **`body`**: The filename of the email body (e.g., `body.html`, `body.txt` or `body.pdf`).
*   **`content_type_of_body`**: The content type of the saved body file (`text/html`, `text/plain`, or `application/pdf`).
*   **`attachment_counts`**: The total number of attachments in the email.
*   **`list_of_attachments`**: A list of objects, where each object represents an attachment and contains the following fields:
    *   `attachment_name_in_message`: The original filename of the attachment.
    *   `content_type_of_attachment`: The MIME type of the attachment.
    *   `size_of_attachment_in_bytes`: The size of the attachment in bytes.
    *   `attachment_name_stored_after_download`: The filename used to save the attachment in the `attachments` folder (e.g., `01_report.pdf`).

### Example `metadata.json`

```json
{
  "to": [
    {
      "emailAddress": {
        "name": "Jane Doe",
        "address": "jane.doe@example.com"
      }
    }
  ],
  "cc": [
    {
      "emailAddress": {
        "name": "John Smith",
        "address": "john.smith@example.com"
      }
    }
  ],
  "from": {
    "emailAddress": {
      "name": "Marketing Team",
      "address": "marketing@example.com"
    }
  },
  "subject": "Q3 Financial Report and Project Updates",
  "received_date": "2024-07-21T14:30:00Z",
  "body": "body.html",
  "content_type_of_body": "text/html",
  "attachment_counts": 3,
  "list_of_attachments": [
    {
      "attachment_name_in_message": "Q3_Financials.xlsx",
      "content_type_of_attachment": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
      "size_of_attachment_in_bytes": 123456,
      "attachment_name_stored_after_download": "01_Q3_Financials.xlsx"
    },
    {
      "attachment_name_in_message": "Project_Timeline.pdf",
      "content_type_of_attachment": "application/pdf",
      "size_of_attachment_in_bytes": 789012,
      "attachment_name_stored_after_download": "02_Project_Timeline.pdf"
    },
    {
      "attachment_name_in_message": "company_logo.png",
      "content_type_of_attachment": "image/png",
      "size_of_attachment_in_bytes": 34567,
      "attachment_name_stored_after_download": "03_company_logo.png"
    }
  ]
}
```

## Token Management

The application requires a valid JWT access token for the Microsoft Graph API. You must provide the token using **exactly one** of the following methods:

| Flag                | Environment Variable | Description                                           |
| ------------------- | -------------------- | ----------------------------------------------------- |
| `-token-string`     |                      | Pass the token directly as a string.                  |
| `-token-file`       |                      | Provide the path to a file containing the token.      |
| `-token-env`        | `JWT_TOKEN`          | Read the token from the `JWT_TOKEN` env var.          |

For added security, you can use the `-remove-token-file` flag to automatically delete the token file after the application finishes.

## Command-line Arguments

All configuration options can be controlled via command-line arguments. Any flag you set will override the corresponding value in the configuration file.

| Argument                        | Description                                                               | Required | Default |
| ------------------------------- | ------------------------------------------------------------------------- | -------- | ------- |
| **Required**                    |                                                                           |          |         |
| `-mailbox`                      | The email address of the mailbox to download. Can also be set in config.  | **Yes**  |         |
| `-workspace`                    | The absolute path for storing artifacts. Required unless `-healthcheck` is used. | Conditional |         |
| **Token (Choose one)**          | Source can be string, file or environment variable                        | **Yes**  |         |
| `-token-string`                 | JWT token as a string.                                                    |          |         |
| `-token-file`                   | Path to a file containing the JWT token.                                  |          |         |
| `-token-env`                    | Read JWT token from the `JWT_TOKEN` environment variable.                 |          | `false` |
| `-remove-token-file`            | Remove the token file after use.                                          | No       | `false` |
| **General**                     |                                                                           |          |         |
| `-config`                       | Path to a JSON configuration file.                                        | No       |         |
| `-debug`                        | Enable debug logging.                                                     | No       | `false` |
| `-healthcheck`                  | Perform a health check and exit.                                          | No       | `false` |
| `-message-details <folder>`     | When used with `-healthcheck`, displays message details for the specified folder. | No       |         |
| `-version`                      | Display the application version and exit.                                 | No       | `false` |
| **Processing & State**          |                                                                           |          |         |
| `-processing-mode`              | Processing mode: `full`, `incremental`, or `route`.                       | No       | `full`  |
| `-inbox-folder`                 | The source folder from which to process messages.                         | No       | `Inbox` |
| `-state`                        | Path to the state file for incremental processing.                        | No       |         |
| `-state-save-interval`          | Save state every N messages during a run.                                 | No       | `100`   |
| `-processed-folder`             | Destination folder for successful messages in `route` mode.               | No       | `Processed`|
| `-error-folder`                 | Destination folder for failed messages in `route` mode.                   | No       | `Error` |
| **Email Body Conversion**       |                                                                           |          |         |
| `-convert-body`                 | Conversion mode for email bodies: `none`, `text`, or `pdf`.               | No       | `none`  |
| `-chromium-path`                | Absolute path to the headless Chromium/Chrome binary (required for `pdf`).| No       |         |
| **Performance & Limits**        |                                                                           |          |         |
| `-parallel-processors`          | Maximum number of parallel message processors.                            | No       | `4`     |
| `-parallel-downloads`           | Maximum number of parallel attachment downloaders.                        | No       | `10`    |
| `-timeout`                      | HTTP client timeout in seconds.                                           | No       | `120`   |
| `-api-rate`                     | API calls per second for client-side rate limiting.                       | No       | `5.0`   |
| `-api-burst`                    | API burst capacity for client-side rate limiting.                         | No       | `10`    |
| `-max-retries`                  | Maximum number of retries for failed API calls.                           | No       | `2`     |
| `-initial-backoff-seconds`      | Initial backoff in seconds for retries.                                   | No       | `5`     |
| `-bandwidth-limit-mbs`          | Bandwidth limit in MB/s for downloads (0 for disabled).                   | No       | `0.0`   |
| `-large-attachment-threshold-mb` | Threshold in MB for what is considered a large attachment.                | No       | `20`    |
| `-chunk-size-mb`                | Chunk size in MB for downloading large attachments.                       | No       | `8`     |

## Configuration File

For a more permanent setup, you can use a JSON file (e.g., `config.json`) and pass its path via the `-config` flag. All options available as flags can be set in the config file.

### Example `config.json`

```json
{
  "mailboxName": "user@example.com",
  "workspacePath": "/path/to/your/output",
  "tokenString": "your-jwt-token-here",
  "debugLogging": false,
  "healthcheck": false,
  "messageDetailsFolder": "",
  "processingMode": "route",
  "stateFilePath": "/path/to/your/state.json",
  "inboxFolder": "Inbox",
  "processedFolder": "Processed-Archive",
  "errorFolder": "Error-Items",
  "httpClientTimeoutSeconds": 120,
  "maxRetries": 2,
  "initialBackoffSeconds": 5,
	"maxParallelProcessors": 4,
	"maxParallelDownloaders": 10,
  "apiCallsPerSecond": 5.0,
  "apiBurst": 10,
  "stateSaveInterval": 100,
  "bandwidthLimitMBs": 0.0,
  "largeAttachmentThresholdMB": 20,
  "chunkSizeMB": 8
}
```

### Configuration Directives

*   **Required**:
    *   `mailboxName`: (String) The email address of the mailbox to download.
    *   `workspacePath`: (String) The absolute path to a unique folder for storing downloaded artifacts.
*   **Token Management**:
    *   `tokenString`: (String) The JWT token.
    *   `tokenFile`: (String) Path to the token file.
    *   `tokenEnv`: (Boolean) Set to `true` to use the `JWT_TOKEN` environment variable.
    *   `removeTokenFile`: (Boolean) Set to `true` to delete the token file after use.
*   **General**:
    *   `debugLogging`: (Boolean) Enables debug-level logging.
    *   `healthcheck`: (Boolean) Set to `true` to perform a health check and exit.
    *   `messageDetailsFolder`: (String) When `healthcheck` is `true`, displays message details for the specified folder.
*   **Processing & State**:
    *   `processingMode`: (String) `full`, `incremental`, or `route`. In `route` mode, messages are moved after processing.
    *   `inboxFolder`: (String) The source folder to process messages from. Defaults to the main `Inbox`.
    *   `stateFilePath`: (String) Absolute path to the state file for incremental mode.
    *   `stateSaveInterval`: (Integer) How often to save the state file during a run (number of messages).
    *   `processedFolder`: (String) The destination folder for successfully processed messages in `route` mode.
    *   `errorFolder`: (String) The destination folder for messages that failed processing in `route` mode.
*   **Performance & Limits**:
    *   `httpClientTimeoutSeconds`: (Integer) Timeout in seconds for HTTP requests.
    *   `maxRetries`: (Integer) The maximum number of retries for failed API calls.
    *   `initialBackoffSeconds`: (Integer) The initial backoff in seconds for retries.
    *   `maxParallelProcessors`: (Integer) The maximum number of concurrent message processors.
    *   `maxParallelDownloaders`: (Integer) The maximum number of concurrent attachment downloaders.
    *   `apiCallsPerSecond`: (Float) The number of API calls allowed per second.
    *   `apiBurst`: (Integer) The burst capacity for the API rate limiter.
    *   `bandwidthLimitMBs`: (Float) The download speed limit in megabytes per second. `0` means disabled.
*   **Attachments**:
    *   `largeAttachmentThresholdMB`: (Integer) Threshold in MB for what is considered a large attachment.
    *   `chunkSizeMB`: (Integer) Chunk size in MB for downloading large attachments.
*   **Email Body Conversion**:
    *   `convertBody`: (String) The conversion mode for email bodies. Can be `none` (no conversion, saves `.html`), `text` (converts to plain text, saves `.txt`), or `pdf` (converts to PDF, saves `.pdf`). Defaults to `none`.
    *   `chromiumPath`: (String) The absolute path to a headless Chromium or Google Chrome binary. This is **required** if `convertBody` is set to `pdf`.

### A Note on API Permissions

For maximum security, it is recommended to use an Azure App Registration with the principle of least privilege.
*   For download-only modes (`full`, `incremental`), the `Mail.Read` permission is sufficient.
*   For `route` mode, which moves emails, the `Mail.ReadWrite` permission is required.

## Examples

### 1. Basic Run with Token String

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-string "YOUR_ACCESS_TOKEN"
```

### 2. Incremental Run Using a Token File

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -remove-token-file \
    -processing-mode incremental \
    -state "/path/to/state.json"
```

### 3. High-Performance Run Using a Config File and Overrides

This example uses a `config.json` file but overrides the concurrency and rate limits with command-line flags. It allocates a small number of processors and a larger number of downloaders.

```shell
./o365mbx \
    -config "/path/to/your/config.json" \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -parallel-processors 8 \
    -parallel-downloads 20 \
    -api-rate 10.0 \
    -api-burst 20
```

### 4. Route Mode with Default Folders

This example processes all messages from the "Inbox", saves the artifacts, and moves the original messages to either a "Processed" or "Error" folder in the mailbox.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -processing-mode route
```

### 5. Route Mode with Custom Folders

This example does the same as above, but moves the messages to custom-named folders.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -processing-mode route \
    -processed-folder "Archive/Succeeded" \
    -error-folder "Archive/Failed"
```

### 6. High-Throughput Download

This example configures the application for maximum download speed by increasing the number of parallel downloaders for I/O-bound tasks and raising the API rate limits.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -parallel-processors 4 \
    -parallel-downloads 30 \
    -api-rate 15.0 \
    -api-burst 30
```

### 7. Bandwidth-Limited Download

This example throttles the total download speed to 50 MB/s to avoid hitting API data egress limits during a very large-scale download.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -bandwidth-limit-mbs 50.0
```

### 8. Body Conversion to Plain Text

This example downloads all emails and converts their bodies from HTML to plain text.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -convert-body text
```

### 9. Health Check Examples

The `-healthcheck` flag provides powerful, read-only tools to inspect the mailbox without downloading any items.

#### Basic Health Check

This example runs a general health check on the mailbox, showing overall stats and a list of all folders, sorted by name.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -token-env \
    -healthcheck
```

**Example Output:**

```text
--- Mailbox Health Check ---
Mailbox: user@example.com
------------------------------
Total Messages: 1573
Total Folders: 14
Total Mailbox Size: 250.75 MB
------------------------------

--- Folder Statistics ---
Folder                 Items   Size (KB)
-------                -----   ---------
Archive                50      102400.00
Conversation History   2       50.20
Deleted Items          10      5120.00
Drafts                 1       10.50
Inbox                  25      81920.00
Junk Email             5       1024.00
Outbox                 0       0.00
... (and so on)
-------------------------
```

#### Message Details Health Check

This example extends the health check to show detailed information for all messages within a specific folder, such as the `Inbox`.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -token-env \
    -healthcheck \
    -message-details "Inbox"
```

**Example Output:**

```text
--- Message Details for Folder: Inbox ---
From                  To                      Date                Subject                                                                 Attachments  Total Size (KB)
----                  --                      ----                -------                                                                 -----------  ----------------
sender@corp.com       user@example.com        2024-08-01 10:30    Q3 Financial Report and Project Updates                                 2            1205.63
another@sender.org    user@example.com;...    2024-08-01 09:15    Important: Action Required for your account                             0            5.12
marketing@company.com user@example.com        2024-07-31 16:00    Weekly Newsletter - Check out our new features and updates for this week! 0            15.30
... (and so on)
-------------------------------------------------
```

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
